package media

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"inno-live-server/internal/metrics"

	"github.com/pion/rtp"
)

// counterValue는 레지스트리를 렌더링해 라벨 없는 카운터 하나의 값을 돌려준다.
// ffmpeg 없이 AudioPipe의 드롭·쓰기 기록을 검증한다.
func counterValue(t *testing.T, reg *metrics.Registry, name string) float64 {
	t.Helper()
	var buf bytes.Buffer
	reg.WritePrometheus(&buf)
	scanner := bufio.NewScanner(&buf)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == name {
			value, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			return value
		}
	}
	return 0
}

const (
	audioWrittenMetric = "innolive_audio_samples_written_total"
	audioDroppedMetric = "innolive_audio_samples_dropped_total"
)

// TestAudioPipeWritePacketDropsOldestWhenFull: RTP 수신 큐는 막히지 않는다. 가득
// 차면 가장 오래된 패킷을 버려 자리를 만들어, 느린 egress가 WebRTC 읽기 루프를
// 멈추지 않게 한다.
func TestAudioPipeWritePacketDropsOldestWhenFull(t *testing.T) {
	reg := metrics.New()
	p := NewAudioPipe(testLogger(), reg, 2)
	// Run은 일부러 시작하지 않아 p.input을 비우는 쪽이 없다.
	for i := 0; i < audioIngressQueueSize; i++ {
		p.WritePacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i)}})
	}
	if got := counterValue(t, reg, audioDroppedMetric); got != 0 {
		t.Fatalf("filling to capacity should not drop, got %v", got)
	}
	p.WritePacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: 9999}}) // 넘침
	if got := counterValue(t, reg, audioDroppedMetric); got != 1 {
		t.Fatalf("overflow should drop exactly one, got %v", got)
	}
	if len(p.input) != audioIngressQueueSize {
		t.Fatalf("queue should stay full at %d, got %d", audioIngressQueueSize, len(p.input))
	}
	if !p.PacketSeen() {
		t.Fatal("PacketSeen must be true after writes")
	}
}

// TestAudioPipeMonotonicGuardDropsBackwardTimestamps: 늘지 않는 PacketTimestamp
// (samplebuilder 강제 flush에서 생길 수 있다)는 oggwriter 전에 버린다. 그러지 않으면
// oggwriter의 uint32 granule 차이가 언더플로해 앞으로 크게 건너뛴다.
func TestAudioPipeMonotonicGuardDropsBackwardTimestamps(t *testing.T) {
	reg := metrics.New()
	p := NewAudioPipe(testLogger(), reg, 2)
	f, err := os.Create(filepath.Join(t.TempDir(), "out.ogg"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Attach(f, false); err != nil {
		t.Fatalf("attach: %v", err)
	}
	payload := []byte{0xf8, 0x01, 0x02, 0x03}
	// 1000,2000 rising → written; 1500 backward, 2000 equal → dropped; 3000 → written.
	for _, ts := range []uint32{1000, 2000, 1500, 2000, 3000} {
		p.writeSample(ts, payload)
	}
	if got := counterValue(t, reg, audioWrittenMetric); got != 3 {
		t.Fatalf("written = %v, want 3", got)
	}
	if got := counterValue(t, reg, audioDroppedMetric); got != 2 {
		t.Fatalf("dropped = %v, want 2", got)
	}
}

// TestAudioPipeDetachIsWriteEndTargeted: 트랙 교체 뒤 정리되는 낡은 egress가 새
// egress가 방금 붙인 스트림을 닫지 못한다. Detach는 일치하는 쓰기 끝에만 작용한다.
func TestAudioPipeDetachIsWriteEndTargeted(t *testing.T) {
	reg := metrics.New()
	p := NewAudioPipe(testLogger(), reg, 2)
	dir := t.TempDir()
	f1, err := os.Create(filepath.Join(dir, "a.ogg"))
	if err != nil {
		t.Fatal(err)
	}
	f2, err := os.Create(filepath.Join(dir, "b.ogg"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Attach(f1, false); err != nil {
		t.Fatalf("attach: %v", err)
	}
	p.Detach(f2) // 다른(낡은) 쓰기 끝 — 아무 일도 없어야 한다
	p.writeSample(1000, []byte{0xf8, 0x01})
	if got := counterValue(t, reg, audioWrittenMetric); got != 1 {
		t.Fatalf("Detach(other) must not detach; written=%v want 1", got)
	}
	p.Detach(f1) // 실제 쓰기 끝 — 떼어 낸다
	p.writeSample(2000, []byte{0xf8, 0x01})
	if got := counterValue(t, reg, audioWrittenMetric); got != 1 {
		t.Fatalf("no write after Detach(f1); written=%v want 1", got)
	}
	if got := counterValue(t, reg, audioDroppedMetric); got != 1 {
		t.Fatalf("write while detached must drop; dropped=%v want 1", got)
	}
}

// TestAudioPipeChannelsDefaultsToStereo: Ogg 헤더에 넣는 채널 수(0 → 2)를 확인한다.
// 페이로드와 어긋난 헤더는 잘못된 속도로 재생된다.
func TestAudioPipeChannelsDefaultsToStereo(t *testing.T) {
	p := NewAudioPipe(testLogger(), metrics.New(), 0)
	if p.Channels() != 2 {
		t.Fatalf("channels=%d, want 2 when constructed with 0", p.Channels())
	}
	p.SetChannels(1)
	if p.Channels() != 1 {
		t.Fatalf("channels=%d, want 1", p.Channels())
	}
	p.SetChannels(0)
	if p.Channels() != 2 {
		t.Fatalf("channels=%d, want 2 when set to 0", p.Channels())
	}
}

// TestAudioPipeAttachWritesOggOpusHeader: Attach는 곧바로 유효한 Ogg/Opus 헤더를
// 내보내, 새로 띄운 egress FFmpeg가 첫 바이트부터 올바른 스트림을 본다.
func TestAudioPipeAttachWritesOggOpusHeader(t *testing.T) {
	p := NewAudioPipe(testLogger(), metrics.New(), 2)
	path := filepath.Join(t.TempDir(), "header.ogg")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Attach(f, false); err != nil {
		t.Fatalf("attach: %v", err)
	}
	p.Detach(f)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 4 || string(data[:4]) != "OggS" {
		t.Fatalf("stream must start with OggS capture pattern, got %q", data[:min(len(data), 4)])
	}
	if !bytes.Contains(data, []byte("OpusHead")) {
		t.Fatal("stream must contain an OpusHead identification header")
	}
}
