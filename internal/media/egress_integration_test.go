//go:build egress_harness

// Integration test for RTMPEgress: drives the real ffmpeg encode+mux path to a
// temp FLV file and asserts the output with ffprobe. Unlike the harness (which
// measures/logs), this test fails on wrong output. It exercises the full egress
// code path — fps measurement, process spawn, frame writing, mux, teardown —
// with real ffmpeg, so it is gated behind the egress_harness build tag and
// skips when ffmpeg/ffprobe are absent.
//
//	go test -tags egress_harness -run TestEgressIntegration ./internal/media -v
package media

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not found in PATH; skipping integration test", name)
	}
}

func ffprobeField(t *testing.T, path, streamSel, entries string) string {
	t.Helper()
	args := []string{"-v", "error"}
	if streamSel != "" {
		args = append(args, "-select_streams", streamSel)
	}
	args = append(args, "-show_entries", entries, "-of", "default=noprint_wrappers=1:nokey=0", path)
	out, err := exec.Command("ffprobe", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestEgressIntegrationFileOutput feeds synthetic JPEG frames through a real
// RTMPEgress writing to a temp FLV and asserts the muxed result: H.264/yuv420p
// video, AAC audio, a plausible duration, and monotonic video DTS.
func TestEgressIntegrationFileOutput(t *testing.T) {
	requireTool(t, "ffmpeg")
	requireTool(t, "ffprobe")

	out := t.TempDir() + "/egress.flv"
	egress := newHarnessEgress(t, out) // jpeg wire format by default
	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup
	done.Add(1)
	go func() { defer done.Done(); egress.Run(ctx) }()

	const fps = 30
	const seconds = 3
	ticker := time.NewTicker(time.Second / fps)
	defer ticker.Stop()
	deadline := time.After(seconds * time.Second)
	timestamp := uint32(90000)
	step := uint32(videoClockRate / fps)
	index := 0
feed:
	for {
		select {
		case <-deadline:
			break feed
		case <-ticker.C:
			egress.Enqueue(frame{
				data:      harnessJPEG(t, index),
				timestamp: timestamp,
				width:     uint16(harnessWidth),
				height:    uint16(harnessHeight),
			})
			timestamp += step
			index++
		}
	}
	cancel()
	done.Wait()

	// --- assertions on the muxed FLV ---
	vinfo := ffprobeField(t, out, "v", "stream=codec_name,pix_fmt")
	if !strings.Contains(vinfo, "codec_name=h264") {
		t.Errorf("video codec not h264:\n%s", vinfo)
	}
	if !strings.Contains(vinfo, "pix_fmt=yuv420p") {
		t.Errorf("video pix_fmt not yuv420p:\n%s", vinfo)
	}

	ainfo := ffprobeField(t, out, "a", "stream=codec_name")
	if !strings.Contains(ainfo, "codec_name=aac") {
		t.Errorf("audio codec not aac (silent AAC track expected):\n%s", ainfo)
	}

	dinfo := ffprobeField(t, out, "", "format=duration")
	dur := parseDuration(t, dinfo)
	if dur < 1.5 {
		t.Errorf("duration %.2fs too short for a %ds feed", dur, seconds)
	}

	assertMonotonicDTS(t, out)
	t.Logf("fed %d frames -> %s: %s %s duration=%.2fs", index, out,
		strings.TrimSpace(strings.ReplaceAll(vinfo, "\n", " ")),
		strings.TrimSpace(ainfo), dur)
}

// TestEgressIntegrationResolutionChange: 트랙 교체를 모사해 프레임 해상도를
// 640x360 → 1280x720으로 바꾸면, egress가 백오프·재연결 집계 없이 FFmpeg를
// 재기동하고 최종 출력이 새 해상도로 muxing되는지 실 ffmpeg로 검증한다(#84).
//
// 고정 시간 급전은 race 계측 환경에서 프레임 공급이 재측정 수(30)에 못 미쳐
// 재스폰 전에 끝나버릴 수 있으므로, "starting RTMP egress" 로그 횟수를 관측해
// 각 세대의 스폰이 실제로 일어난 뒤에만 다음 단계로 넘어간다. JPEG 인코딩도
// 급전 루프 밖에서 미리 해둔다(루프 안 동기 인코딩은 race 계측 시 33ms 틱을
// 넘겨 공급 부족을 일으킨 원인이었다).
func TestEgressIntegrationResolutionChange(t *testing.T) {
	requireTool(t, "ffmpeg")
	requireTool(t, "ffprobe")

	logs := &syncLogBuffer{}
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, logs), &slog.HandlerOptions{Level: slog.LevelDebug}))
	out := t.TempDir() + "/egress-resolution.flv"
	egress := NewRTMPEgress("ffmpeg", logger, metrics.New(), TranscoderOptions{WireFormat: config.WireFormatJPEG}, out, nil, false, 0, "")
	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup
	done.Add(1)
	go func() { defer done.Done(); egress.Run(ctx) }()

	const fps = 30
	smallFrame := sizedJPEG(t, 0, 640, 360)
	bigFrame := sizedJPEG(t, 1, 1280, 720)
	spawnCount := func() int { return strings.Count(logs.String(), "starting RTMP egress") }

	timestamp := uint32(90000)
	step := uint32(videoClockRate / fps)
	// feedUntil은 조건이 참이 될 때까지 프레임을 공급하고, 조건 달성 후에도
	// extra 프레임을 더 밀어 새로 뜬 FFmpeg가 유효한 출력을 쓰게 한다.
	feedUntil := func(data []byte, width, height uint16, condition func() bool, extra int, label string) {
		t.Helper()
		ticker := time.NewTicker(time.Second / fps)
		defer ticker.Stop()
		deadline := time.After(60 * time.Second)
		remaining := -1
		for {
			select {
			case <-deadline:
				t.Fatalf("%s: condition not reached within 60s (spawns=%d)", label, spawnCount())
			case <-ticker.C:
				egress.Enqueue(frame{data: data, timestamp: timestamp, width: width, height: height})
				timestamp += step
				if remaining < 0 && condition() {
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

	// 1세대: 첫 스폰이 관측될 때까지 640x360 공급.
	feedUntil(smallFrame, 640, 360, func() bool { return spawnCount() >= 1 }, 5, "first spawn")
	// 2세대: 해상도 불일치 → 재측정 → 두 번째 스폰이 관측될 때까지 1280x720
	// 공급하고, FLV에 실제 프레임이 muxing되도록 1초 분량을 더 밀어준다.
	feedUntil(bigFrame, 1280, 720, func() bool { return spawnCount() >= 2 }, fps, "respawn after resolution change")
	cancel()
	done.Wait()

	vinfo := ffprobeField(t, out, "v", "stream=width,height,codec_name")
	if !strings.Contains(vinfo, "width=1280") || !strings.Contains(vinfo, "height=720") {
		t.Errorf("final FLV is not 1280x720 (egress did not respawn for the new resolution):\n%s", vinfo)
	} else if !strings.Contains(vinfo, "codec_name=h264") {
		t.Errorf("video codec not h264:\n%s", vinfo)
	} else {
		t.Logf("resolution change verified: %s", strings.TrimSpace(strings.ReplaceAll(vinfo, "\n", " ")))
	}
	// 해상도 재기동은 송출 실패가 아니다: 재연결로 집계되면 안 된다.
	if status := egress.Status(); status.ReconnectAttempts != 0 {
		t.Errorf("resolution restart was counted as reconnect: attempts=%d", status.ReconnectAttempts)
	}
}

// syncLogBuffer는 여러 고루틴이 쓰는 slog 출력을 race 없이 모으는 버퍼다.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// sizedJPEG는 지정 해상도의 합성 JPEG 프레임을 만든다. harnessJPEG와 달리
// 전역 해상도(EGRESS_WIDTH/HEIGHT)에 묶이지 않아 해상도 전환 시나리오에 쓴다.
func sizedJPEG(t *testing.T, index, width, height int) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			canvas.Set(x, y, color.RGBA{R: uint8(x / 3), G: uint8(y / 2), B: uint8(index * 2), A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, canvas, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encode sized frame: %v", err)
	}
	return encoded.Bytes()
}

func parseDuration(t *testing.T, ffprobeOut string) float64 {
	t.Helper()
	for _, line := range strings.Split(ffprobeOut, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "duration="); ok {
			d, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("parse duration %q: %v", v, err)
			}
			return d
		}
	}
	t.Fatalf("no duration in ffprobe output: %s", ffprobeOut)
	return 0
}

// assertMonotonicDTS fails if any video packet DTS is not strictly increasing —
// YouTube drops connections on non-monotonic DTS.
func assertMonotonicDTS(t *testing.T, path string) {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v",
		"-show_entries", "packet=dts_time", "-of", "csv=p=0", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe dts failed: %v\n%s", err, out)
	}
	prev := -1.0
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "N/A" {
			continue
		}
		dts, err := strconv.ParseFloat(line, 64)
		if err != nil {
			continue
		}
		if dts <= prev {
			t.Fatalf("non-monotonic DTS: %.4f after %.4f (packet %d)", dts, prev, count)
		}
		prev = dts
		count++
	}
	if count == 0 {
		t.Fatal("no video packets found in output")
	}
}
