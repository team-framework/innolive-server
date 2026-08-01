//go:build egress_harness

// Integration tests for the audio egress path with real ffmpeg/ffprobe. Unlike
// the harness (which measures), these assert output and fail on regressions.
// They cover behaviour not exercised by egress_audio_harness_test.go:
//   - -itsoffset (EGRESS_AUDIO_OFFSET_MS) actually delays the audio stream
//   - an attached-but-silent AudioPipe (no mic packets) falls back to the
//     generated-silence path instead of stalling on an empty Ogg input
//
//	go test -tags egress_harness -run TestEgressAudioIntegration ./internal/media -v -timeout 5m
package media

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"inno-live-server/internal/metrics"

	"github.com/pion/rtp"
)

// feedEgress drives an egress with synthetic video (and, when audioFeed is
// non-nil, real Opus audio) for the given duration, then tears it down.
func feedEgress(t *testing.T, egress *RTMPEgress, seconds int, audioFeed func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup
	done.Add(1)
	go func() { defer done.Done(); egress.Run(ctx) }()

	videoTicker := time.NewTicker(time.Second / 30)
	audioTicker := time.NewTicker(20 * time.Millisecond)
	defer videoTicker.Stop()
	defer audioTicker.Stop()
	deadline := time.After(time.Duration(seconds) * time.Second)
	timestamp := uint32(90000)
	index := 0
	for {
		select {
		case <-deadline:
			cancel()
			done.Wait()
			return
		case <-videoTicker.C:
			egress.Enqueue(frame{
				data:      harnessFrameData(t, index, harnessWireFormat()),
				timestamp: timestamp,
				stageAt:   time.Now(),
				width:     harnessWidth,
				height:    harnessHeight,
			})
			timestamp += uint32(videoClockRate / 30)
			index++
		case <-audioTicker.C:
			if audioFeed != nil {
				audioFeed()
			}
		}
	}
}

func parseStartTime(t *testing.T, ffprobeOut string) float64 {
	t.Helper()
	_, value, ok := strings.Cut(strings.TrimSpace(ffprobeOut), "=")
	if !ok {
		t.Fatalf("no start_time in %q", ffprobeOut)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		t.Fatalf("parse start_time %q: %v", value, err)
	}
	return seconds
}

// TestEgressAudioIntegrationItsOffset asserts a positive EGRESS_AUDIO_OFFSET_MS
// delays the audio relative to the video in the muxed FLV, which is how A/V
// sync is corrected for the blur-delayed video.
func TestEgressAudioIntegrationItsOffset(t *testing.T) {
	requireTool(t, "ffmpeg")
	requireTool(t, "ffprobe")
	payloads := buildSineOpusPayloads(t)
	out := t.TempDir() + "/out.flv"

	pipe := NewAudioPipe(testLogger(), metrics.New(), 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pipe.Run(ctx)

	var ts uint32 = 480000
	var seq uint16 = 1000
	feed := func() {
		pipe.WritePacket(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: seq, Timestamp: ts, SSRC: 7},
			Payload: payloads[int(seq)%len(payloads)],
		})
		ts += 960
		seq++
	}
	feed()
	feed()

	const offset = 400 * time.Millisecond
	egress := NewRTMPEgress("ffmpeg", testLogger(), metrics.New(),
		TranscoderOptions{WireFormat: harnessWireFormat()}, out, pipe, false, offset, "")
	feedEgress(t, egress, 6, feed)

	videoStart := parseStartTime(t, ffprobeField(t, out, "v", "stream=start_time"))
	audioStart := parseStartTime(t, ffprobeField(t, out, "a", "stream=start_time"))
	// With a 400 ms audio offset the audio timeline must start clearly later
	// than the (wallclock, ~0) video timeline. Loose bound absorbs pipeline jitter.
	if audioStart-videoStart < 0.15 {
		t.Fatalf("audio not delayed by offset: video_start=%.3f audio_start=%.3f (want audio-video >= 0.15s for %v offset)",
			videoStart, audioStart, offset)
	}
	t.Logf("itsoffset %v → video_start=%.3f audio_start=%.3f", offset, videoStart, audioStart)
}

// TestEgressAudioIntegrationNoMicFallsBackToSilence proves an attached AudioPipe
// that never receives a microphone packet does not stall the egress on an empty
// Ogg input: PacketSeen() stays false so start() picks the silence path, and the
// FLV still carries both an h264 video and an aac audio stream.
func TestEgressAudioIntegrationNoMicFallsBackToSilence(t *testing.T) {
	requireTool(t, "ffmpeg")
	requireTool(t, "ffprobe")
	out := t.TempDir() + "/out.flv"

	// Pipe present and running, but no WritePacket is ever called.
	pipe := NewAudioPipe(testLogger(), metrics.New(), 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pipe.Run(ctx)
	if pipe.PacketSeen() {
		t.Fatal("PacketSeen should be false before any mic packet")
	}

	egress := NewRTMPEgress("ffmpeg", testLogger(), metrics.New(),
		TranscoderOptions{WireFormat: harnessWireFormat()}, out, pipe, false, 0, "")
	feedEgress(t, egress, 5, nil)

	codecs := ffprobeAll(t, out, "stream=codec_name")
	if !strings.Contains(codecs, "h264") || !strings.Contains(codecs, "aac") {
		t.Fatalf("silence fallback must still mux h264+aac, got: %s", strings.ReplaceAll(codecs, "\n", ","))
	}
}
