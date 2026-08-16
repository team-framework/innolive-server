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
	// opusClockRate는 채널 수와 관계없이 WebRTC Opus에 고정된 값이다.
	opusClockRate = 48000
	// audioReorderMaxLate는 samplebuilder의 재정렬 범위를 제한한다(20ms 패킷
	// 약 20개, 약 400ms).
	audioReorderMaxLate  = 20
	audioReorderMaxDelay = 400 * time.Millisecond
	// audioIngressQueueSize는 트랙 읽기 루프와 pipe writer 사이의 RTP 패킷을
	// 버퍼링한다. FFmpeg가 느려도 RTP 수신을 막지 않으며, 가득 차면 가장 오래된
	// 패킷을 버린다.
	audioIngressQueueSize = 256
	// opusSilenceFrameDuration은 방송 일시 중지 중 기록하는 Opus 무음 프레임의
	// 길이다. Opus clock rate에 맞춰 증가해야 FFmpeg가 단조로운 Ogg/Opus
	// 타임라인을 계속 받을 수 있다.
	opusSilenceFrameDuration = 20 * time.Millisecond
)

var opusSilenceFrame = []byte{0xf8, 0xff, 0xfe}

// AudioPipe는 송출자의 Opus 마이크 입력을 WebRTC ingest에서 RTMP egress FFmpeg로
// Ogg/Opus 바이트 스트림으로 전달한다. RTP 읽기 루프는 패킷을 블로킹 없는 버퍼
// 채널에 넣고, 단일 writer 고루틴이 samplebuilder로 재정렬한 뒤 oggwriter로
// 컨테이너화한다.
//
// egress FFmpeg 자식 프로세스는 독립적으로 다시 생성될 수 있으므로 writer 출력은
// mutex로 교체한다. egress가 새 pipe를 Attach할 때까지는 분리된 상태로 샘플을
// 버리고, Attach마다 새 Ogg 헤더를 쓰는 oggwriter를 만들어 모든 FFmpeg 프로세스가
// 첫 바이트부터 유효한 스트림을 받게 한다.
type AudioPipe struct {
	logger   *slog.Logger
	metrics  *metrics.Registry
	channels atomic.Uint32

	input   chan *rtp.Packet
	builder *samplebuilder.SampleBuilder

	packetSeen          atomic.Bool
	lastPacketTimestamp atomic.Uint32
	muted               atomic.Bool

	mu            sync.Mutex
	ogg           *oggwriter.OggWriter
	writeEnd      *os.File
	havePrev      bool
	prevTimestamp uint32
	pipeErrLogged bool
}

// NewAudioPipe는 Opus 트랙이 지정한 채널 수(track.Codec().Channels)를 사용하는
// 송출자용 AudioPipe를 만든다.
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

// SetChannels는 협상된 codec의 Opus 채널 수를 기록한다. Ogg 헤더가 스트림과
// 일치하도록 첫 Attach 전에 설정해야 하며, payload와 다른 헤더는 잘못된 속도로
// 재생된다.
func (p *AudioPipe) SetChannels(channels uint16) {
	if channels == 0 {
		channels = 2
	}
	p.channels.Store(uint32(channels))
}

// Channels는 현재 Opus 채널 수를 반환한다.
func (p *AudioPipe) Channels() uint16 { return uint16(p.channels.Load()) }

// WritePacket은 RTP 읽기 루프를 막지 않고 RTP 패킷 하나를 writer 고루틴에
// 전달한다. 큐가 가득 차면 가장 오래된 패킷을 먼저 버린다.
func (p *AudioPipe) WritePacket(packet *rtp.Packet) {
	p.packetSeen.Store(true)
	p.lastPacketTimestamp.Store(packet.Timestamp)
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

// PacketSeen은 Opus RTP 패킷을 하나 이상 받았는지 반환한다. egress는 이를
// 실제 마이크와 무음 중 어느 입력을 사용할지 결정하는 데 사용한다.
func (p *AudioPipe) PacketSeen() bool { return p.packetSeen.Load() }

// SetMuted는 연결된 egress 오디오를 발행자 마이크와 생성한 Opus 무음 사이에서
// 전환한다. mute 상태는 egress 수명에 속하므로 FFmpeg가 재연결되어도 유지한다.
func (p *AudioPipe) SetMuted(muted bool) { p.muted.Store(muted) }

// Muted는 실제 마이크 샘플을 현재 막고 있는지 반환한다.
func (p *AudioPipe) Muted() bool { return p.muted.Load() }

// Run은 단일 writer 고루틴을 실행한다. 패킷을 재정렬해 완성된 Opus 샘플을 현재
// 연결된 Ogg 스트림에 기록하고, 분리된 동안에는 샘플을 버린다. ctx가 취소되면
// (세션 종료) 반환한다.
func (p *AudioPipe) Run(ctx context.Context) {
	ticker := time.NewTicker(opusSilenceFrameDuration)
	defer ticker.Stop()
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
		case <-ticker.C:
			if p.Muted() {
				p.writeSilenceSample()
			}
		}
	}
}

// writeSample은 oggwriter가 요구하는 최소 RTP 패킷(Timestamp와 Payload만 사용)을
// 재구성해 연결된 Ogg 스트림에 추가한다. 강제 samplebuilder flush에서 생길 수 있는
// 비단조 timestamp는 버려, oggwriter의 uint32 granule delta가 언더플로로 매우 큰
// 미래 시점으로 뛰지 않게 한다.
func (p *AudioPipe) writeSample(timestamp uint32, payload []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Muted() {
		p.metrics.IncAudioSampleDropped()
		return
	}
	p.writeSampleLocked(timestamp, payload)
}

// writeSilenceSample은 유효한 20ms Opus 무음 프레임 하나를 추가한다. 마이크
// 패킷과 같은 timestamp 흐름을 사용하므로 재개 뒤 oggwriter가 과거 시점으로
// 되돌아간 것으로 판단하지 않는다.
func (p *AudioPipe) writeSilenceSample() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.Muted() {
		return
	}
	timestamp := uint32(0)
	if p.havePrev {
		timestamp = p.prevTimestamp + uint32(opusClockRate*opusSilenceFrameDuration/time.Second)
	} else if p.PacketSeen() {
		// 아직 samplebuilder가 첫 샘플을 내보내기 전 pause된 경우에도, WebRTC
		// RTP timestamp 근처에서 시작해 이후 실제 마이크 패킷과 큰 간격이 나지
		// 않게 한다.
		timestamp = p.lastPacketTimestamp.Load()
	}
	p.writeSampleLocked(timestamp, opusSilenceFrame)
}

func (p *AudioPipe) writeSampleLocked(timestamp uint32, payload []byte) {
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

// Attach는 writeEnd 위에 새 Ogg 스트림을 만들어 pipe를 새로 생성된 egress
// FFmpeg에 연결한다. 자식 프로세스가 즉시 유효한 스트림을 받도록 Ogg/Opus 헤더를
// 동기적으로 기록한다. 호출자는 이후 Detach를 통해서만 writeEnd를 닫는다.
func (p *AudioPipe) Attach(writeEnd *os.File) error {
	ogg, err := oggwriter.NewWith(writeEnd, opusClockRate, p.Channels())
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// 이전 연결이 아직 닫히기 전에 재연결되는 경우처럼, 남아 있는 스트림을 교체한다.
	p.detachLocked()
	p.ogg = ogg
	p.writeEnd = writeEnd
	p.havePrev = false
	p.pipeErrLogged = false
	return nil
}

// Detach는 writeEnd에 연결된 Ogg 스트림을 닫아 해당 egress FFmpeg가 pipe:3에서
// EOF를 받게 한다. 특정 write end만 대상으로 하므로 트랙 교체 뒤 종료되는 이전
// egress가 새 egress가 연결한 스트림을 닫을 수 없다. 멱등적이며 연결되지 않은
// 상태에서도 안전하게 호출할 수 있다.
func (p *AudioPipe) Detach(writeEnd *os.File) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.writeEnd != writeEnd {
		return
	}
	p.detachLocked()
}

// closeCurrent는 현재 연결된 스트림을 종료한다. 호출자가 특정 write end를 추적하지
// 않는 세션 종료에 사용한다.
func (p *AudioPipe) closeCurrent() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.detachLocked()
}

func (p *AudioPipe) detachLocked() {
	if p.ogg == nil {
		return
	}
	// Close는 스트림 모드에서 하위 io.Writer(pipe write end)를 닫는다.
	_ = p.ogg.Close()
	p.ogg = nil
	p.writeEnd = nil
}
