//go:build egress_harness

// 3단계 오디오 egress 검증. 합성 영상 프레임과 실제 Opus 마이크(ffmpeg로 미리
// 인코딩한 사인파를 fd 3의 오디오 파이프로 RTP 패킷처럼 재생)로 실제 RTMPEgress를
// 돌린다. 이후 ffprobe가 FLV 출력에서 h264 영상과 aac 오디오 스트림을 모두 보고해야
// 한다 — ExtraFiles/pipe:3 배선과 Ogg 먹싱을 끝까지 증명한다.
//
//	go test -tags egress_harness -run TestEgressHarnessAudio ./internal/media -v -timeout 5m
package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"inno-live-server/internal/metrics"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

// buildSineOpusPayloads는 ffmpeg로 사인파를 Ogg/Opus로 미리 인코딩하고, 페이지마다
// 페이로드를 RTP로 재생할 원시 Opus 패킷으로 돌려준다.
func buildSineOpusPayloads(t *testing.T) [][]byte {
	dir := t.TempDir()
	path := filepath.Join(dir, "sine.ogg")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6",
		"-c:a", "libopus", "-b:a", "64k", "-ar", "48000", "-ac", "2",
		// Ogg 페이지마다 Opus 프레임 하나라 ParseNextPage 페이로드가 곧 RTP로 재생할
		// 패킷 하나다.
		"-page_duration", "20000", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("encode sine opus: %v\n%s", err, out)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, _, err := oggreader.NewWith(file)
	if err != nil {
		t.Fatalf("open ogg: %v", err)
	}
	var payloads [][]byte
	for {
		payload, _, err := reader.ParseNextPage()
		if err != nil {
			break
		}
		if len(payload) == 0 {
			continue
		}
		buf := make([]byte, len(payload))
		copy(buf, payload)
		payloads = append(payloads, buf)
	}
	if len(payloads) < 50 {
		t.Fatalf("expected many opus pages, got %d", len(payloads))
	}
	return payloads
}

func TestEgressHarnessAudio(t *testing.T) {
	output := os.Getenv("EGRESS_OUT")
	if output == "" {
		output = filepath.Join(t.TempDir(), "audio.flv")
	}
	fps := harnessEnvInt(t, "EGRESS_FPS", 30)
	seconds := harnessEnvInt(t, "EGRESS_SECONDS", 5)
	payloads := buildSineOpusPayloads(t)

	pipe := NewAudioPipe(testLogger(), metrics.New(), 2)
	ctx, cancel := context.WithCancel(context.Background())
	go pipe.Run(ctx)

	// 파이프를 미리 채워 egress가 시작할 때 PacketSeen() 확인에서 무음이 아니라
	// 마이크 경로를 고르게 한다.
	var audioTS uint32 = 480000
	var audioSeq uint16 = 1000
	feedAudio := func() {
		pipe.WritePacket(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: audioSeq, Timestamp: audioTS, SSRC: 7},
			Payload: payloads[int(audioSeq)%len(payloads)],
		})
		audioTS += 960
		audioSeq++
	}
	feedAudio()
	feedAudio()

	// newHarnessEgress는 오디오를 nil로 넘기므로 파이프를 붙인 것을 따로 만든다.
	egress := NewRTMPEgress("ffmpeg", testLogger(), metrics.New(),
		TranscoderOptions{WireFormat: harnessWireFormat()}, output, pipe, false, 0, "", "")

	var done sync.WaitGroup
	done.Add(1)
	go func() { defer done.Done(); egress.Run(ctx) }()

	videoTicker := time.NewTicker(time.Second / time.Duration(fps))
	audioTicker := time.NewTicker(20 * time.Millisecond)
	defer videoTicker.Stop()
	defer audioTicker.Stop()
	deadline := time.After(time.Duration(seconds) * time.Second)
	timestamp := uint32(90000)
	step := uint32(videoClockRate / fps)
	index := 0
feed:
	for {
		select {
		case <-deadline:
			break feed
		case <-videoTicker.C:
			egress.Enqueue(frame{
				data:      harnessFrameData(t, index, harnessWireFormat()),
				timestamp: timestamp,
				stageAt:   time.Now(),
				width:     uint16(harnessWidth),
				height:    uint16(harnessHeight),
			})
			timestamp += step
			index++
		case <-audioTicker.C:
			feedAudio()
		}
	}
	cancel()
	done.Wait()

	// FLV에는 영상과 오디오 스트림이 모두 있어야 한다.
	types := ffprobeAll(t, output, "stream=codec_type")
	if !strings.Contains(types, "video") || !strings.Contains(types, "audio") {
		t.Fatalf("stream types = %q, want both video and audio", types)
	}
	acodec := ffprobeAll(t, output, "stream=codec_name")
	if !strings.Contains(acodec, "aac") || !strings.Contains(acodec, "h264") {
		t.Fatalf("codecs = %q, want h264 and aac", acodec)
	}
	t.Logf("egress FLV codecs: %s", strings.ReplaceAll(acodec, "\n", ","))
}

func ffprobeAll(t *testing.T, path, entry string) string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", entry, "-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", entry, err)
	}
	return strings.TrimSpace(string(out))
}
