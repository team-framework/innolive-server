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

// counterValue renders the registry and returns a single unlabelled counter's
// value, so AudioPipe's drop/write bookkeeping can be asserted without ffmpeg.
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

// TestAudioPipeWritePacketDropsOldestWhenFull verifies the RTP ingress queue
// never blocks: once full, the oldest packet is dropped to make room so the
// WebRTC read loop is never stalled by a slow egress.
func TestAudioPipeWritePacketDropsOldestWhenFull(t *testing.T) {
	reg := metrics.New()
	p := NewAudioPipe(testLogger(), reg, 2)
	// Run is deliberately not started, so nothing drains p.input.
	for i := 0; i < audioIngressQueueSize; i++ {
		p.WritePacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i)}})
	}
	if got := counterValue(t, reg, audioDroppedMetric); got != 0 {
		t.Fatalf("filling to capacity should not drop, got %v", got)
	}
	p.WritePacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: 9999}}) // overflow
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

// TestAudioPipeMonotonicGuardDropsBackwardTimestamps ensures a non-increasing
// PacketTimestamp (possible on a forced samplebuilder flush) is dropped before
// oggwriter, whose uint32 granule delta would otherwise underflow into a huge
// forward jump.
func TestAudioPipeMonotonicGuardDropsBackwardTimestamps(t *testing.T) {
	reg := metrics.New()
	p := NewAudioPipe(testLogger(), reg, 2)
	f, err := os.Create(filepath.Join(t.TempDir(), "out.ogg"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Attach(f); err != nil {
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

// TestAudioPipeDetachIsWriteEndTargeted proves a stale egress tearing down after
// a track replacement cannot close the stream a newer egress just attached:
// Detach only acts on the matching write end.
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
	if err := p.Attach(f1); err != nil {
		t.Fatalf("attach: %v", err)
	}
	p.Detach(f2) // some other (stale) write end — must be a no-op
	p.writeSample(1000, []byte{0xf8, 0x01})
	if got := counterValue(t, reg, audioWrittenMetric); got != 1 {
		t.Fatalf("Detach(other) must not detach; written=%v want 1", got)
	}
	p.Detach(f1) // the real write end — detaches
	p.writeSample(2000, []byte{0xf8, 0x01})
	if got := counterValue(t, reg, audioWrittenMetric); got != 1 {
		t.Fatalf("no write after Detach(f1); written=%v want 1", got)
	}
	if got := counterValue(t, reg, audioDroppedMetric); got != 1 {
		t.Fatalf("write while detached must drop; dropped=%v want 1", got)
	}
}

// TestAudioPipeChannelsDefaultsToStereo checks the channel count fed to the Ogg
// header (0 → 2), since a header that disagrees with the payload plays back at
// the wrong speed.
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

// TestAudioPipeAttachWritesOggOpusHeader confirms Attach emits a valid
// Ogg/Opus header immediately, so a freshly spawned egress FFmpeg sees a
// well-formed stream from its first byte.
func TestAudioPipeAttachWritesOggOpusHeader(t *testing.T) {
	p := NewAudioPipe(testLogger(), metrics.New(), 2)
	path := filepath.Join(t.TempDir(), "header.ogg")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Attach(f); err != nil {
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
