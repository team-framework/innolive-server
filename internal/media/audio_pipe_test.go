package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"inno-live-server/internal/metrics"

	"github.com/pion/rtp"
)

// TestAudioPipeProducesValidOgg feeds synthetic Opus RTP packets through the
// pipe into a file and asserts ffprobe reads back a well-formed Ogg/Opus stream
// at 48 kHz with a plausible duration. This is the Phase 2 verification: the
// container and granule timeline are correct even before real egress.
func TestAudioPipeProducesValidOgg(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "out.ogg")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	pipe := NewAudioPipe(testLogger(), metrics.New(), 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pipe.Run(ctx)

	if err := pipe.Attach(file); err != nil {
		t.Fatalf("attach: %v", err)
	}

	// 100 Opus packets, 20 ms apart (960 samples @ 48 kHz) ≈ 2 s of audio.
	const packets = 100
	var timestamp uint32 = 160000
	for seq := 0; seq < packets; seq++ {
		pipe.WritePacket(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: uint16(seq), Timestamp: timestamp, SSRC: 1},
			Payload: []byte{0xfc, 0x01, 0x02, 0x03},
		})
		timestamp += 960
	}

	// Drain the writer goroutine, then close the stream so the file flushes.
	deadline := time.Now().Add(2 * time.Second)
	for len(pipe.input) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	pipe.Detach(file)

	codec := ffprobeValue(t, path, "stream=codec_name")
	if codec != "opus" {
		t.Fatalf("codec_name = %q, want opus", codec)
	}
	rate := ffprobeValue(t, path, "stream=sample_rate")
	if rate != "48000" {
		t.Fatalf("sample_rate = %q, want 48000", rate)
	}
	duration := ffprobeValue(t, path, "format=duration")
	if seconds, _ := time.ParseDuration(duration + "s"); seconds < 1500*time.Millisecond {
		t.Fatalf("duration = %q, want >= ~1.5s", duration)
	}
}

func ffprobeValue(t *testing.T, path, entry string) string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", entry, "-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", entry, err)
	}
	return strings.TrimSpace(string(out))
}
