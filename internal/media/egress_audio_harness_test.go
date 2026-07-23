//go:build egress_harness

// Phase 3 audio-egress verification. Drives the real RTMPEgress with synthetic
// video frames AND a real Opus microphone (a sine wave pre-encoded by ffmpeg,
// replayed as RTP packets through the audio pipe on fd 3). Afterwards ffprobe
// must report both an h264 video stream and an aac audio stream in the FLV
// output — proving the ExtraFiles/pipe:3 wiring and Ogg muxing end to end.
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

// buildSineOpusPayloads pre-encodes a sine wave to Ogg/Opus with ffmpeg and
// returns each page's payload as a raw Opus packet to replay over RTP.
func buildSineOpusPayloads(t *testing.T) [][]byte {
	dir := t.TempDir()
	path := filepath.Join(dir, "sine.ogg")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6",
		"-c:a", "libopus", "-b:a", "64k", "-ar", "48000", "-ac", "2",
		// One Opus frame per Ogg page so each ParseNextPage payload is a single
		// packet we can replay over RTP.
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

	// Prime the pipe so the egress's spawn-time PacketSeen() check picks the
	// microphone path rather than silence.
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

	// newHarnessEgress passes nil audio; build one with the pipe attached.
	egress := NewRTMPEgress("ffmpeg", testLogger(), metrics.New(),
		TranscoderOptions{WireFormat: harnessWireFormat()}, output, pipe, false, 0)

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
				width:     harnessWidth,
				height:    harnessHeight,
			})
			timestamp += step
			index++
		case <-audioTicker.C:
			feedAudio()
		}
	}
	cancel()
	done.Wait()

	// FLV must carry both a video and an audio stream.
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
