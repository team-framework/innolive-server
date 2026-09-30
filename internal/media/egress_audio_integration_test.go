//go:build egress_harness

// 실제 ffmpeg/ffprobe로 오디오 egress 경로를 검증하는 통합 테스트다. 측정용인
// 하네스와 달리 출력을 단언하고 회귀가 있으면 실패한다. egress_audio_harness_test.go가
// 다루지 않는 동작을 본다.
//
//   - -itsoffset(EGRESS_AUDIO_OFFSET_MS)이 실제로 오디오 스트림을 늦춘다
//
//   - 붙어 있지만 조용한 AudioPipe(마이크 패킷 없음)는 빈 Ogg 입력에서 멈추지 않고
//     생성한 무음 경로로 넘어간다
//
//     go test -tags egress_harness -run TestEgressAudioIntegration ./internal/media -v -timeout 5m
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

// feedEgress는 주어진 시간 동안 합성 영상(audioFeed가 있으면 실제 Opus 오디오도)으로
// egress를 돌린 뒤 정리한다.
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
				width:     uint16(harnessWidth),
				height:    uint16(harnessHeight),
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

// TestEgressAudioIntegrationItsOffset: 양수 EGRESS_AUDIO_OFFSET_MS는 먹싱한 FLV에서
// 오디오를 영상보다 늦춘다. 블러로 늦어진 영상에 A/V 싱크를 맞추는 방법이다.
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
		TranscoderOptions{WireFormat: harnessWireFormat()}, out, pipe, false, offset, "", "")
	feedEgress(t, egress, 6, feed)

	videoStart := parseStartTime(t, ffprobeField(t, out, "v", "stream=start_time"))
	audioStart := parseStartTime(t, ffprobeField(t, out, "a", "stream=start_time"))
	// 오디오 오프셋이 400ms면 오디오 타임라인은 (벽시계, 약 0인) 영상 타임라인보다
	// 분명히 늦게 시작해야 한다. 느슨한 한계로 파이프라인 지터를 흡수한다.
	if audioStart-videoStart < 0.15 {
		t.Fatalf("audio not delayed by offset: video_start=%.3f audio_start=%.3f (want audio-video >= 0.15s for %v offset)",
			videoStart, audioStart, offset)
	}
	t.Logf("itsoffset %v → video_start=%.3f audio_start=%.3f", offset, videoStart, audioStart)
}

// TestEgressAudioIntegrationNoMicFallsBackToSilence: 마이크 패킷을 한 번도 받지 않은
// AudioPipe가 붙어 있어도 egress가 빈 Ogg 입력에서 멈추지 않는다. PacketSeen()이
// false로 남아 start()가 무음 경로를 고르고, FLV에는 h264 영상과 aac 오디오가
// 모두 실린다.
func TestEgressAudioIntegrationNoMicFallsBackToSilence(t *testing.T) {
	requireTool(t, "ffmpeg")
	requireTool(t, "ffprobe")
	out := t.TempDir() + "/out.flv"

	// 파이프는 있고 돌지만 WritePacket은 한 번도 부르지 않는다.
	pipe := NewAudioPipe(testLogger(), metrics.New(), 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pipe.Run(ctx)
	if pipe.PacketSeen() {
		t.Fatal("PacketSeen should be false before any mic packet")
	}

	egress := NewRTMPEgress("ffmpeg", testLogger(), metrics.New(),
		TranscoderOptions{WireFormat: harnessWireFormat()}, out, pipe, false, 0, "", "")
	feedEgress(t, egress, 5, nil)

	codecs := ffprobeAll(t, out, "stream=codec_name")
	if !strings.Contains(codecs, "h264") || !strings.Contains(codecs, "aac") {
		t.Fatalf("silence fallback must still mux h264+aac, got: %s", strings.ReplaceAll(codecs, "\n", ","))
	}
}
