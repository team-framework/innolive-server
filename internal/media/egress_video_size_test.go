//go:build egress_harness

// EGRESS_VIDEO_SIZE(송출 해상도 고정) 회귀 테스트. 실 ffmpeg/ffprobe를 쓰므로
// egress_harness 빌드 태그 뒤에 두고, 도구가 없으면 건너뛴다.
//
//	go test -tags egress_harness -run TestEgressVideoSize ./internal/media -v
package media

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

// TestEgressVideoSizePinsOutputResolution은 #121의 회귀 테스트다.
//
// 프로덕션 H.264 경로를 그대로 모사한다: 프레임 페이로드(JPEG)는 640x360에서
// 1280x720으로 실제로 커지지만, frame.width/height 메타데이터는 최초 SPS 값에
// 고정된 채 남는다(#122). 이 상태에서 resolutionChanged는 울리지 않으므로
// 해상도 재기동(#84)이 발동하지 않는다. EGRESS_VIDEO_SIZE로 출력 해상도를
// 고정하면 그럼에도 송출 해상도가 유지되어야 한다.
//
// 대조군인 TestEgressIntegrationResolutionChange는 메타데이터도 함께 바꾸므로
// 재기동 경로로 통과한다. 여기서는 재기동 없이(spawns == 1) 통과해야 한다 —
// RTMP 재연결이 발생하면 방송이 끊기기 때문이다.
func TestEgressVideoSizePinsOutputResolution(t *testing.T) {
	requireTool(t, "ffmpeg")
	requireTool(t, "ffprobe")

	logs := &syncLogBuffer{}
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, logs), &slog.HandlerOptions{Level: slog.LevelDebug}))
	out := t.TempDir() + "/egress-video-size.flv"
	egress := NewRTMPEgress("ffmpeg", logger, metrics.New(),
		TranscoderOptions{WireFormat: config.WireFormatJPEG}, out, nil, false, 0, "", "1280x720")

	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup
	done.Add(1)
	go func() { defer done.Done(); egress.Run(ctx) }()

	const fps = 30
	rampFrame := sizedJPEG(t, 0, 640, 360)
	fullFrame := sizedJPEG(t, 1, 1280, 720)
	spawnCount := func() int { return strings.Count(logs.String(), "starting RTMP egress") }

	timestamp := uint32(90000)
	step := uint32(videoClockRate / fps)
	// feed는 stop이 참이 될 때까지 프레임을 공급하고, 그 뒤 extra 프레임을 더
	// 밀어 FFmpeg가 실제로 muxing할 분량을 확보한다. 고정 시간 급전은 race
	// 계측 환경에서 재측정 수(30)를 못 채울 수 있어 조건 관측 방식을 쓴다.
	feed := func(data []byte, width, height uint16, stop func() bool, extra int, label string) {
		t.Helper()
		ticker := time.NewTicker(time.Second / fps)
		defer ticker.Stop()
		deadline := time.After(60 * time.Second)
		remaining := -1
		for {
			select {
			case <-deadline:
				t.Fatalf("%s: 조건에 도달하지 못했다 (spawns=%d)", label, spawnCount())
			case <-ticker.C:
				egress.Enqueue(frame{data: data, timestamp: timestamp, width: width, height: height})
				timestamp += step
				if remaining < 0 && stop() {
					remaining = extra
					continue
				}
				if remaining > 0 {
					remaining--
				}
				if remaining == 0 {
					return
				}
			}
		}
	}

	// 1) 램프업 초기 저해상도로 첫 스폰을 만든다.
	feed(rampFrame, 640, 360, func() bool { return spawnCount() >= 1 }, 5, "first spawn")
	// 2) 퍼블리셔가 720p로 올라간다. 페이로드는 1280x720이지만 메타데이터는
	//    프로덕션과 동일하게 640x360으로 고정된 채 흘려보낸다.
	sent := 0
	feed(fullFrame, 640, 360, func() bool { sent++; return sent >= 2*fps }, fps, "720p payload with stale metadata")

	cancel()
	done.Wait()

	if spawns := spawnCount(); spawns != 1 {
		t.Errorf("해상도 고정 상태에서는 재기동이 없어야 한다 (RTMP 재연결 = 방송 단절): spawns=%d", spawns)
	}
	vinfo := ffprobeField(t, out, "v", "stream=width,height,codec_name")
	summary := strings.TrimSpace(strings.ReplaceAll(vinfo, "\n", " "))
	if !strings.Contains(vinfo, "width=1280") || !strings.Contains(vinfo, "height=720") {
		t.Errorf("송출 결과가 고정 해상도 1280x720이 아니다: %s", summary)
	} else if !strings.Contains(vinfo, "codec_name=h264") {
		t.Errorf("video codec not h264: %s", summary)
	} else {
		t.Logf("해상도 고정 확인: %s", summary)
	}
}

// TestEgressVideoSizeUpscalesRampUpFrames는 입력이 고정 해상도보다 작을 때
// 업스케일로 채워 출력 프로필이 유지되는지 확인한다. 방송 시작 직후 몇 초는
// 흐릿하지만 YouTube가 인식하는 인입 해상도는 처음부터 고정값이어야 한다.
func TestEgressVideoSizeUpscalesRampUpFrames(t *testing.T) {
	requireTool(t, "ffmpeg")
	requireTool(t, "ffprobe")

	out := t.TempDir() + "/egress-upscale.flv"
	egress := NewRTMPEgress("ffmpeg", testLogger(), metrics.New(),
		TranscoderOptions{WireFormat: config.WireFormatJPEG}, out, nil, false, 0, "", "1280x720")

	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup
	done.Add(1)
	go func() { defer done.Done(); egress.Run(ctx) }()

	const fps = 30
	small := sizedJPEG(t, 0, 320, 180)
	ticker := time.NewTicker(time.Second / fps)
	defer ticker.Stop()
	timestamp := uint32(90000)
	step := uint32(videoClockRate / fps)
	for i := 0; i < 3*fps; i++ {
		<-ticker.C
		egress.Enqueue(frame{data: small, timestamp: timestamp, width: 320, height: 180})
		timestamp += step
	}
	cancel()
	done.Wait()

	vinfo := ffprobeField(t, out, "v", "stream=width,height")
	if !strings.Contains(vinfo, "width=1280") || !strings.Contains(vinfo, "height=720") {
		t.Errorf("320x180 입력이 고정 해상도로 업스케일되지 않았다: %s",
			strings.TrimSpace(strings.ReplaceAll(vinfo, "\n", " ")))
	}
}
