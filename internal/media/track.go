package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

const (
	defaultFrameDuration     = time.Second / 30
	videoClockRate           = 90000
	rtpReorderMaxLatePackets = 512
	rtpReorderMaxDelay       = 250 * time.Millisecond
	rtpIngressQueueSize      = 1024
	maxTrackedMissingPackets = 2048
	// keyframeRequestMinInterval throttles keyframe requests: one lost gap
	// usually spans several samples, and the publisher needs time to encode
	// and deliver the keyframe before another request is worth sending.
	keyframeRequestMinInterval = 500 * time.Millisecond
)

type frame struct {
	data      []byte
	timestamp uint32
	stageAt   time.Time
	// ingestAt is when this frame was first assembled from RTP, preserved
	// unchanged through decode/blur/encode so the egress can measure the full
	// ingest→pipe-write latency (the blur pipeline delay that A/V sync must
	// compensate for). Unlike stageAt it is never reset.
	ingestAt time.Time
	width    uint16
	height   uint16
}

// EgressSlot은 실행 중인 파이프라인에 egress를 나중에 꽂거나 뗄 수 있게 하는
// 홀더다. 명시적 송출 시작(#83)은 트랙 도착(파이프라인 기동) 이후에 오므로,
// 파이프라인은 egress를 직접 들지 않고 이 슬롯을 통해서만 참조한다.
type EgressSlot struct {
	current atomic.Pointer[RTMPEgress]
}

func NewEgressSlot() *EgressSlot { return &EgressSlot{} }

// Set은 활성 egress를 교체한다.
func (s *EgressSlot) Set(egress *RTMPEgress) { s.current.Store(egress) }

// Clear는 슬롯을 비운다. 이후 파이프라인 프레임은 egress로 가지 않는다.
func (s *EgressSlot) Clear() { s.current.Store(nil) }

// ClearIf는 현재 슬롯이 expected를 가리킬 때만 비운다. 이전 egress Run 고루틴이
// 종료되는 사이 새 방송이 같은 슬롯에 설치될 수 있으므로, 무조건 Clear하면 새
// 방송까지 끊길 수 있다.
func (s *EgressSlot) ClearIf(expected *RTMPEgress) bool {
	if s == nil {
		return false
	}
	return s.current.CompareAndSwap(expected, nil)
}

// Load는 현재 활성 egress를 돌려준다(슬롯이 nil이거나 비었으면 nil).
func (s *EgressSlot) Load() *RTMPEgress {
	if s == nil {
		return nil
	}
	return s.current.Load()
}

type rtpSequenceObservation struct {
	gap        uint64
	recovered  bool
	outOfOrder bool
}

type rtpSequenceTracker struct {
	initialized bool
	expected    uint16
	missing     map[uint16]struct{}
}

func (t *rtpSequenceTracker) observe(sequence uint16) rtpSequenceObservation {
	if !t.initialized {
		t.initialized = true
		t.expected = sequence + 1
		t.missing = make(map[uint16]struct{})
		return rtpSequenceObservation{}
	}

	if sequence == t.expected {
		t.expected++
		return rtpSequenceObservation{}
	}

	delta := int16(sequence - t.expected)
	if delta > 0 {
		gap := uint64(uint16(delta))
		if gap <= maxTrackedMissingPackets {
			for missing := t.expected; missing != sequence; missing++ {
				t.missing[missing] = struct{}{}
			}
		} else {
			clear(t.missing)
		}
		t.expected = sequence + 1
		return rtpSequenceObservation{gap: gap}
	}

	if _, ok := t.missing[sequence]; ok {
		delete(t.missing, sequence)
		return rtpSequenceObservation{recovered: true}
	}
	return rtpSequenceObservation{outOfOrder: true}
}

type rtpFrameAssembler struct {
	builder  *samplebuilder.SampleBuilder
	registry *metrics.Registry
	mode     string
	// requestKeyframe asks the publisher for a fresh keyframe. It may be nil
	// when no feedback channel is available.
	requestKeyframe     func()
	keyframeMinInterval time.Duration
	lastKeyframeAt      time.Time
}

func newRTPFrameAssembler(registry *metrics.Registry, mode config.PrivacyMode, codec VideoCodec, requestKeyframe func()) (*rtpFrameAssembler, error) {
	assembler, err := newRTPFrameAssemblerWithLimits(
		registry,
		mode,
		codec,
		rtpReorderMaxLatePackets,
		rtpReorderMaxDelay,
	)
	if err != nil {
		return nil, err
	}
	assembler.requestKeyframe = requestKeyframe
	assembler.keyframeMinInterval = keyframeRequestMinInterval
	return assembler, nil
}

func newRTPFrameAssemblerWithLimits(
	registry *metrics.Registry,
	mode config.PrivacyMode,
	codec VideoCodec,
	maxLatePackets uint16,
	maxLateDelay time.Duration,
) (*rtpFrameAssembler, error) {
	var depacketizer rtp.Depacketizer
	switch codec {
	case VideoCodecVP8:
		depacketizer = &codecs.VP8Packet{}
	case VideoCodecH264:
		depacketizer = &codecs.H264Packet{}
	default:
		return nil, fmt.Errorf("unsupported video codec %q", codec)
	}
	return &rtpFrameAssembler{
		builder: samplebuilder.New(
			maxLatePackets,
			depacketizer,
			videoClockRate,
			samplebuilder.WithMaxTimeDelay(maxLateDelay),
		),
		registry: registry,
		mode:     string(mode),
	}, nil
}

func (a *rtpFrameAssembler) push(packet *rtp.Packet) []frame {
	a.builder.Push(packet)
	return a.pop()
}

func (a *rtpFrameAssembler) flush() []frame {
	a.builder.Flush()
	return a.pop()
}

func (a *rtpFrameAssembler) pop() []frame {
	var frames []frame
	for sample := a.builder.Pop(); sample != nil; sample = a.builder.Pop() {
		if sample.PrevDroppedPackets > 0 {
			a.registry.AddRTPPacketsDiscarded(a.mode, uint64(sample.PrevDroppedPackets))
			a.requestKeyframeAfterLoss()
		}
		a.registry.IncRTPFramesAssembled(a.mode)
		now := time.Now()
		frames = append(frames, frame{
			data:      sample.Data,
			timestamp: sample.PacketTimestamp,
			stageAt:   now,
			ingestAt:  now,
		})
	}
	return frames
}

// requestKeyframeAfterLoss asks the publisher for a keyframe once the sample
// builder has given up on a gap. Dropped packets mean the decoder just lost its
// reference, and every frame the pipeline re-encodes from that point carries
// the damage — including its own keyframes. Without this the picture stays
// broken until the publisher happens to send a keyframe on its own.
func (a *rtpFrameAssembler) requestKeyframeAfterLoss() {
	if a.requestKeyframe == nil {
		return
	}
	now := time.Now()
	if !a.lastKeyframeAt.IsZero() && now.Sub(a.lastKeyframeAt) < a.keyframeMinInterval {
		return
	}
	a.lastKeyframeAt = now
	a.requestKeyframe()
}

func RunTrack(
	ctx context.Context,
	logger *slog.Logger,
	remote *webrtc.TrackRemote,
	local *webrtc.TrackLocalStaticSample,
	processor *Processor,
	transcoder *FFmpegTranscoder,
	egress *EgressSlot,
	registry *metrics.Registry,
	mode config.PrivacyMode,
	queueSize int,
	requestKeyframe func(),
) {
	defer processor.Close()
	runTranscodedTrack(ctx, logger, remote, local, processor, transcoder, egress, registry, mode, queueSize, requestKeyframe)
}

func runTranscodedTrack(
	ctx context.Context,
	logger *slog.Logger,
	remote *webrtc.TrackRemote,
	local *webrtc.TrackLocalStaticSample,
	processor *Processor,
	transcoder *FFmpegTranscoder,
	egress *EgressSlot,
	registry *metrics.Registry,
	mode config.PrivacyMode,
	queueSize int,
	requestKeyframe func(),
) {
	if transcoder == nil {
		registry.IncFrameFailure(string(mode))
		logger.Error("privacy mode requires an FFmpeg transcoder", "mode", mode)
		return
	}
	pipelineContext, cancel := context.WithCancel(ctx)
	compressedQueueSize := queueSize * 4
	if compressedQueueSize < 8 {
		compressedQueueSize = 8
	}
	compressed := make(chan frame, compressedQueueSize)
	decoderOutput := make(chan frame, compressedQueueSize)
	decoded := make(chan frame, queueSize)
	processed := make(chan frame, 1)
	encoded := make(chan frame, compressedQueueSize)

	go readFrames(pipelineContext, logger, remote, compressed, registry, mode, requestKeyframe)
	var streamWorkers sync.WaitGroup
	streamWorkers.Add(2)
	go func() {
		defer streamWorkers.Done()
		err := transcoder.DecodeStream(pipelineContext, compressed, decoderOutput)
		close(decoderOutput)
		if err != nil && pipelineContext.Err() == nil {
			registry.IncFrameFailure(string(mode))
			logger.Error("FFmpeg decode stream failed", "error", err)
			cancel()
		}
	}()
	var queueWorkers sync.WaitGroup
	queueWorkers.Add(2)
	go func() {
		defer queueWorkers.Done()
		keepLatestDecoded(pipelineContext, decoderOutput, decoded, registry, mode)
	}()
	go func() {
		defer queueWorkers.Done()
		processImages(pipelineContext, logger, processor, decoded, processed, egress, registry, mode)
	}()
	go func() {
		defer streamWorkers.Done()
		err := transcoder.EncodeStream(pipelineContext, processed, encoded)
		close(encoded)
		if err != nil && pipelineContext.Err() == nil {
			registry.IncFrameFailure(string(mode))
			logger.Error("FFmpeg encode stream failed", "error", err)
			cancel()
		}
	}()
	defer func() {
		cancel()
		queueWorkers.Wait()
		// Join the FFmpeg-owning goroutines so RunTrack's return means the
		// process pair has actually finished its (graceful) teardown — the
		// session manager's capacity accounting relies on this.
		streamWorkers.Wait()
		subtractRemainingQueue(decoded, registry)
	}()

	var previousOutputTimestamp uint32
	var haveOutputTimestamp bool
	for {
		select {
		case <-pipelineContext.Done():
			return
		case item, ok := <-encoded:
			if !ok {
				return
			}
			registry.ObserveStage("encode", time.Since(item.stageAt))
			duration := durationSincePrevious(item.timestamp, &previousOutputTimestamp, &haveOutputTimestamp)
			if !writeOutputFrame(pipelineContext, logger, local, registry, mode, item.data, duration) {
				return
			}
		}
	}
}

func keepLatestDecoded(ctx context.Context, input <-chan frame, decoded chan frame, registry *metrics.Registry, mode config.PrivacyMode) {
	defer close(decoded)
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-input:
			if !ok {
				return
			}
			registry.ObserveStage("decode", time.Since(item.stageAt))
			if !enqueueLatestDecoded(ctx, decoded, item, registry, mode) {
				return
			}
		}
	}
}

func enqueueLatestDecoded(
	ctx context.Context,
	decoded chan frame,
	item frame,
	registry *metrics.Registry,
	mode config.PrivacyMode,
) bool {
	registry.AddQueue(1)
	select {
	case decoded <- item:
		return true
	case <-ctx.Done():
		registry.AddQueue(-1)
		return false
	default:
		registry.AddQueue(-1)
	}

	select {
	case <-decoded:
		registry.AddQueue(-1)
		registry.IncFrameDropped(string(mode))
	default:
	}

	registry.AddQueue(1)
	select {
	case decoded <- item:
		return true
	case <-ctx.Done():
		registry.AddQueue(-1)
		return false
	}
}

func processImages(
	ctx context.Context,
	logger *slog.Logger,
	processor *Processor,
	decoded <-chan frame,
	processed chan<- frame,
	egress *EgressSlot,
	registry *metrics.Registry,
	mode config.PrivacyMode,
) {
	defer close(processed)
	var failureLog failureLogger
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-decoded:
			if !ok {
				return
			}
			registry.AddQueue(-1)
			registry.IncFrameReceived(string(mode))
			output, processedByAI, err := processor.ProcessIfAIInputEnabled(ctx, item.data, time.Now().UnixNano(), item.width, item.height)
			if !processedByAI {
				registry.IncAIInputPausedFrame(string(mode))
				continue
			}
			if err != nil {
				recordFrameFailure(ctx, logger, registry, mode, &failureLog, err)
				continue
			}
			registry.IncFrameProcessed(string(mode))
			item.data = output
			item.stageAt = time.Now()
			if sink := egress.Load(); sink != nil {
				sink.Enqueue(item)
			}
			select {
			case processed <- item:
			case <-ctx.Done():
				return
			}
		}
	}
}

func readFrames(
	ctx context.Context,
	logger *slog.Logger,
	remote *webrtc.TrackRemote,
	frames chan frame,
	registry *metrics.Registry,
	mode config.PrivacyMode,
	requestKeyframe func(),
) {
	defer close(frames)
	packets := make(chan *rtp.Packet, rtpIngressQueueSize)
	go readRTPPackets(ctx, logger, remote, packets, registry, mode)

	codec := VideoCodec(remote.Codec().MimeType)
	assembler, err := newRTPFrameAssembler(registry, mode, codec, requestKeyframe)
	if err != nil {
		logger.Error("unsupported WebRTC video codec", "codec", remote.Codec().MimeType)
		return
	}
	for packet := range packets {
		if !emitFrames(ctx, frames, assembler.push(packet)) {
			return
		}
	}
	if ctx.Err() == nil {
		emitFrames(ctx, frames, assembler.flush())
	}
}

func readRTPPackets(
	ctx context.Context,
	logger *slog.Logger,
	remote *webrtc.TrackRemote,
	packets chan *rtp.Packet,
	registry *metrics.Registry,
	mode config.PrivacyMode,
) {
	defer close(packets)
	modeName := string(mode)
	var tracker rtpSequenceTracker
	for {
		packet, _, err := remote.ReadRTP()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, io.EOF) {
				logger.Error("WebRTC RTP read failed", "error", err)
			}
			return
		}

		observeRTPPacket(registry, modeName, &tracker, packet)
		if !enqueueLatestRTPPacket(ctx, packets, packet, registry, modeName) {
			return
		}
	}
}

func observeRTPPacket(
	registry *metrics.Registry,
	mode string,
	tracker *rtpSequenceTracker,
	packet *rtp.Packet,
) {
	registry.IncRTPPacketsReceived(mode)
	observation := tracker.observe(packet.SequenceNumber)
	if observation.gap > 0 {
		registry.AddRTPSequenceGaps(mode, observation.gap)
	}
	if observation.recovered {
		registry.IncRTPPacketsRecovered(mode)
	}
	if observation.outOfOrder {
		registry.IncRTPOutOfOrder(mode)
	}
}

func enqueueLatestRTPPacket(
	ctx context.Context,
	output chan *rtp.Packet,
	packet *rtp.Packet,
	registry *metrics.Registry,
	mode string,
) bool {
	select {
	case output <- packet:
		return true
	case <-ctx.Done():
		return false
	default:
	}

	select {
	case <-output:
		registry.IncRTPIngressPacketDropped(mode)
	default:
	}

	select {
	case output <- packet:
		return true
	case <-ctx.Done():
		return false
	}
}

func emitFrames(ctx context.Context, output chan<- frame, completed []frame) bool {
	for _, item := range completed {
		select {
		case output <- item:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

type failureLogger struct {
	lastLog    time.Time
	suppressed uint64
}

func recordFrameFailure(ctx context.Context, logger *slog.Logger, registry *metrics.Registry, mode config.PrivacyMode, state *failureLogger, err error) {
	if ctx.Err() != nil {
		return
	}
	registry.IncFrameFailure(string(mode))
	if time.Since(state.lastLog) >= time.Second {
		logger.Error("video frame processing failed", "error", err, "suppressed_since_last_log", state.suppressed)
		state.lastLog = time.Now()
		state.suppressed = 0
		return
	}
	state.suppressed++
}

func writeOutputFrame(ctx context.Context, logger *slog.Logger, local *webrtc.TrackLocalStaticSample, registry *metrics.Registry, mode config.PrivacyMode, output []byte, duration time.Duration) bool {
	if err := local.WriteSample(media.Sample{Data: output, Duration: duration}); err != nil {
		if ctx.Err() == nil {
			registry.IncFrameFailure(string(mode))
			logger.Error("processed WebRTC frame write failed", "error", err)
		}
		return false
	}
	return true
}

func durationSincePrevious(timestamp uint32, previous *uint32, initialized *bool) time.Duration {
	duration := defaultFrameDuration
	if *initialized {
		delta := timestamp - *previous
		candidate := time.Duration(float64(delta) / videoClockRate * float64(time.Second))
		if candidate >= time.Millisecond && candidate <= time.Second {
			duration = candidate
		}
	}
	*previous = timestamp
	*initialized = true
	return duration
}

func subtractRemainingQueue[T any](queue <-chan T, registry *metrics.Registry) {
	remaining := int64(len(queue))
	if remaining > 0 {
		registry.AddQueue(-remaining)
	}
}
