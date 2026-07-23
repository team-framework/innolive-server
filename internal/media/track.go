package media

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
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
}

func newRTPFrameAssembler(registry *metrics.Registry, mode config.PrivacyMode) *rtpFrameAssembler {
	return newRTPFrameAssemblerWithLimits(
		registry,
		mode,
		rtpReorderMaxLatePackets,
		rtpReorderMaxDelay,
	)
}

func newRTPFrameAssemblerWithLimits(
	registry *metrics.Registry,
	mode config.PrivacyMode,
	maxLatePackets uint16,
	maxLateDelay time.Duration,
) *rtpFrameAssembler {
	return &rtpFrameAssembler{
		builder: samplebuilder.New(
			maxLatePackets,
			&codecs.VP8Packet{},
			videoClockRate,
			samplebuilder.WithMaxTimeDelay(maxLateDelay),
		),
		registry: registry,
		mode:     string(mode),
	}
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

func RunTrack(
	ctx context.Context,
	logger *slog.Logger,
	remote *webrtc.TrackRemote,
	local *webrtc.TrackLocalStaticSample,
	processor *Processor,
	transcoder *FFmpegTranscoder,
	egress *RTMPEgress,
	registry *metrics.Registry,
	mode config.PrivacyMode,
	queueSize int,
) {
	defer processor.Close()
	runTranscodedTrack(ctx, logger, remote, local, processor, transcoder, egress, registry, mode, queueSize)
}

func runTranscodedTrack(
	ctx context.Context,
	logger *slog.Logger,
	remote *webrtc.TrackRemote,
	local *webrtc.TrackLocalStaticSample,
	processor *Processor,
	transcoder *FFmpegTranscoder,
	egress *RTMPEgress,
	registry *metrics.Registry,
	mode config.PrivacyMode,
	queueSize int,
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

	go readFrames(pipelineContext, logger, remote, compressed, registry, mode)
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
	egress *RTMPEgress,
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
			output, err := processor.Process(ctx, item.data, time.Now().UnixNano(), item.width, item.height)
			if err != nil {
				recordFrameFailure(ctx, logger, registry, mode, &failureLog, err)
				continue
			}
			registry.IncFrameProcessed(string(mode))
			item.data = output
			item.stageAt = time.Now()
			if egress != nil {
				egress.Enqueue(item)
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
) {
	defer close(frames)
	packets := make(chan *rtp.Packet, rtpIngressQueueSize)
	go readRTPPackets(ctx, logger, remote, packets, registry, mode)

	assembler := newRTPFrameAssembler(registry, mode)
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
