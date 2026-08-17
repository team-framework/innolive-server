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

// TestAudioPipeProducesValidOgg는 합성 Opus RTP 패킷을 pipe로 파일에 기록하고,
// ffprobe가 그 결과를 타당한 길이의 48kHz Ogg/Opus 스트림으로 읽는지 확인한다.
// 실제 egress 전에도 컨테이너와 granule 타임라인이 올바른지 검증한다.
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

	// 20ms 간격의 Opus 패킷 100개(48kHz에서 960 samples)는 약 2초 오디오다.
	const packets = 100
	var timestamp uint32 = 160000
	for seq := 0; seq < packets; seq++ {
		pipe.WritePacket(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: uint16(seq), Timestamp: timestamp, SSRC: 1},
			Payload: []byte{0xfc, 0x01, 0x02, 0x03},
		})
		timestamp += 960
	}

	// writer 고루틴을 비운 뒤 스트림을 닫아 파일을 flush한다.
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

func TestMutedAudioPipeProducesDecodableSilenceOgg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	path := filepath.Join(t.TempDir(), "muted.ogg")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	pipe := NewAudioPipe(testLogger(), metrics.New(), 2)
	if err := pipe.Attach(file); err != nil {
		t.Fatal(err)
	}
	pipe.SetMuted(true)
	for index := 0; index < 75; index++ {
		pipe.writeSilenceSample()
	}
	pipe.Detach(file)

	output, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("muted Ogg is not decodable: %v\n%s", err, output)
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
