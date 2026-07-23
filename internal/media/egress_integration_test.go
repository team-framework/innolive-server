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
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
				width:     harnessWidth,
				height:    harnessHeight,
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
