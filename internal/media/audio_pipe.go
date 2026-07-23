package media

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"inno-live-server/internal/metrics"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

const (
	// opusClockRate is fixed for WebRTC Opus regardless of channel count.
	opusClockRate = 48000
	// audioReorderMaxLate bounds the samplebuilder reorder window (~20 Opus
	// packets ≈ 400 ms at 20 ms/packet).
	audioReorderMaxLate  = 20
	audioReorderMaxDelay = 400 * time.Millisecond
	// audioIngressQueueSize buffers RTP packets between the track read loop and
	// the pipe writer so a slow FFmpeg never blocks RTP reception. When full the
	// oldest packet is dropped.
	audioIngressQueueSize = 256
)

// AudioPipe carries the publisher's Opus microphone from the WebRTC ingest to
// the RTMP egress FFmpeg as an Ogg/Opus byte stream. The RTP read loop feeds
// packets through a non-blocking buffered channel; a single writer goroutine
// reorders them with a samplebuilder and containerises them with an oggwriter.
//
// The egress FFmpeg child is (re)spawned independently, so the writer's output
// is swapped under a mutex: it is detached (samples dropped) until the egress
// Attaches a fresh pipe, and a new oggwriter — writing fresh Ogg headers — is
// created per attach so every FFmpeg process receives a valid stream from its
// first byte.
type AudioPipe struct {
	logger   *slog.Logger
	metrics  *metrics.Registry
	channels atomic.Uint32

	input   chan *rtp.Packet
	builder *samplebuilder.SampleBuilder

	packetSeen atomic.Bool

	mu            sync.Mutex
	ogg           *oggwriter.OggWriter
	writeEnd      *os.File
	havePrev      bool
	prevTimestamp uint32
	pipeErrLogged bool
}

// NewAudioPipe builds an AudioPipe for a publisher whose Opus track declares
// the given channel count (from track.Codec().Channels).
func NewAudioPipe(logger *slog.Logger, registry *metrics.Registry, channels uint16) *AudioPipe {
	p := &AudioPipe{
		logger:  logger.With("component", "audio_pipe"),
		metrics: registry,
		input:   make(chan *rtp.Packet, audioIngressQueueSize),
		builder: samplebuilder.New(
			audioReorderMaxLate,
			&codecs.OpusPacket{},
			opusClockRate,
			samplebuilder.WithMaxTimeDelay(audioReorderMaxDelay),
		),
	}
	p.SetChannels(channels)
	return p
}

// SetChannels records the Opus channel count from the negotiated codec. It must
// be set before the first Attach so the Ogg header matches the stream; a header
// that disagrees with the payload plays back at the wrong speed.
func (p *AudioPipe) SetChannels(channels uint16) {
	if channels == 0 {
		channels = 2
	}
	p.channels.Store(uint32(channels))
}

// Channels returns the current Opus channel count.
func (p *AudioPipe) Channels() uint16 { return uint16(p.channels.Load()) }

// WritePacket hands one RTP packet to the writer goroutine without ever
// blocking the RTP read loop: when the queue is full the oldest packet is
// dropped first.
func (p *AudioPipe) WritePacket(packet *rtp.Packet) {
	p.packetSeen.Store(true)
	select {
	case p.input <- packet:
		return
	default:
	}
	select {
	case <-p.input:
		p.metrics.IncAudioSampleDropped()
	default:
	}
	select {
	case p.input <- packet:
	default:
		p.metrics.IncAudioSampleDropped()
	}
}

// PacketSeen reports whether at least one Opus RTP packet has been received.
// The egress uses this to decide between the real microphone and silence.
func (p *AudioPipe) PacketSeen() bool { return p.packetSeen.Load() }

// Run owns the single writer goroutine. It reorders packets and writes each
// completed Opus sample to the currently attached Ogg stream, dropping samples
// while detached. It returns when ctx is cancelled (session teardown).
func (p *AudioPipe) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			p.closeCurrent()
			return
		case packet := <-p.input:
			p.builder.Push(packet)
			for sample := p.builder.Pop(); sample != nil; sample = p.builder.Pop() {
				p.writeSample(sample.PacketTimestamp, sample.Data)
			}
		}
	}
}

// writeSample reconstructs the minimal RTP packet oggwriter expects (it reads
// only Timestamp and Payload) and appends it to the attached Ogg stream. A
// non-monotonic timestamp — possible on a forced samplebuilder flush — is
// dropped so oggwriter's uint32 granule delta never underflows into a huge
// forward jump.
func (p *AudioPipe) writeSample(timestamp uint32, payload []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ogg == nil {
		p.metrics.IncAudioSampleDropped()
		return
	}
	if p.havePrev && int32(timestamp-p.prevTimestamp) <= 0 {
		p.metrics.IncAudioSampleDropped()
		return
	}
	err := p.ogg.WriteRTP(&rtp.Packet{
		Header:  rtp.Header{Timestamp: timestamp},
		Payload: payload,
	})
	if err != nil {
		p.metrics.IncAudioSampleDropped()
		if !p.pipeErrLogged {
			p.logger.Warn("audio pipe write failed; dropping until egress reattaches", "error", err)
			p.pipeErrLogged = true
		}
		p.detachLocked()
		return
	}
	p.prevTimestamp = timestamp
	p.havePrev = true
	p.metrics.IncAudioSampleWritten()
}

// Attach binds the pipe to a freshly spawned egress FFmpeg by creating a new
// Ogg stream over writeEnd. It writes the Ogg/Opus headers synchronously so the
// child sees a valid stream immediately. The caller owns closing writeEnd only
// via a later Detach.
func (p *AudioPipe) Attach(writeEnd *os.File) error {
	ogg, err := oggwriter.NewWith(writeEnd, opusClockRate, p.Channels())
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Replace any stale stream (e.g. a reconnect racing an unclosed prior one).
	p.detachLocked()
	p.ogg = ogg
	p.writeEnd = writeEnd
	p.havePrev = false
	p.pipeErrLogged = false
	return nil
}

// Detach closes the Ogg stream bound to writeEnd, giving that egress FFmpeg an
// EOF on pipe:3. It targets a specific write end so a stale egress tearing down
// after a track replacement cannot close the stream a newer egress just
// attached. It is idempotent and safe to call when unattached.
func (p *AudioPipe) Detach(writeEnd *os.File) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.writeEnd != writeEnd {
		return
	}
	p.detachLocked()
}

// closeCurrent tears down whatever stream is attached, used on session teardown
// where the caller does not track the specific write end.
func (p *AudioPipe) closeCurrent() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.detachLocked()
}

func (p *AudioPipe) detachLocked() {
	if p.ogg == nil {
		return
	}
	// Close closes the underlying io.Writer (the pipe write end) in stream mode.
	_ = p.ogg.Close()
	p.ogg = nil
	p.writeEnd = nil
}
