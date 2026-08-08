package media

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

func newTestEgress(wire config.WireFormat, url string) *RTMPEgress {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewRTMPEgress("ffmpeg", logger, metrics.New(), TranscoderOptions{WireFormat: wire}, url, nil, false, 0, "")
}

func TestVideoBitrateFor(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "out.flv")
	cases := []struct {
		width, height uint16
		want          string
	}{
		{640, 360, egressVideoBitrate},
		{1280, 720, egressVideoBitrate},
		{1920, 1080, egressVideoBitrateFHD},
		{1080, 1920, egressVideoBitrateFHD}, // portrait FHD
	}
	for _, c := range cases {
		if got := e.videoBitrateFor(c.width, c.height); got != c.want {
			t.Errorf("videoBitrateFor(%d, %d) = %q, want %q", c.width, c.height, got, c.want)
		}
	}

	override := newTestEgress(config.WireFormatJPEG, "out.flv")
	override.bitrateOverride = "3000k"
	if got := override.videoBitrateFor(1920, 1080); got != "3000k" {
		t.Errorf("override videoBitrateFor(1920, 1080) = %q, want %q", got, "3000k")
	}
}

func TestMeasureFPS(t *testing.T) {
	// framesAt builds n frames whose RTP timestamps advance by step (90kHz clock).
	framesAt := func(n int, start uint32, step uint32) []frame {
		out := make([]frame, n)
		ts := start
		for i := range out {
			out[i] = frame{timestamp: ts}
			ts += step
		}
		return out
	}

	tests := []struct {
		name  string
		input []frame
		want  int
	}{
		{"30fps", framesAt(31, 0, videoClockRate/30), 30},
		{"60fps", framesAt(31, 0, videoClockRate/60), 60},
		{"24fps", framesAt(25, 1000, videoClockRate/24), 24},
		{"single frame falls back", framesAt(1, 0, 3000), egressDefaultFPS},
		{"zero span falls back", framesAt(10, 5000, 0), egressDefaultFPS},
		{"too high falls back", framesAt(10, 0, videoClockRate/180), egressDefaultFPS},
		{"timestamp wraparound", framesAt(31, 0xFFFFFC18, videoClockRate/30), 30},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := measureFPS(tc.input); got != tc.want {
				t.Fatalf("measureFPS = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMaskStreamKey(t *testing.T) {
	tests := map[string]string{
		"rtmp://a.rtmp.youtube.com/live2/ss0y-abcd-1234": "rtmp://a.rtmp.youtube.com/live2/****",
		"rtmp://host/live/key":                           "rtmp://host/live/****",
		"rtmp://host/live/":                              "rtmp://host/live/", // trailing slash: nothing to mask
		"out.flv":                                        "out.flv",           // no slash
	}
	for in, want := range tests {
		if got := maskStreamKey(in); got != want {
			t.Errorf("maskStreamKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsProgressKey(t *testing.T) {
	progress := []string{"frame", "fps", "bitrate", "out_time_ms", "dup_frames", "drop_frames", "speed", "progress", "stream_0_0_q"}
	for _, k := range progress {
		if !isProgressKey(k) {
			t.Errorf("isProgressKey(%q) = false, want true", k)
		}
	}
	notProgress := []string{"Connection refused", "[rtmp @ 0x1]", "Error", "broken pipe"}
	for _, k := range notProgress {
		if isProgressKey(k) {
			t.Errorf("isProgressKey(%q) = true, want false", k)
		}
	}
}

func TestValidFrame(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0x00, 0x11, 0xFF, 0xD9}
	notJPEG := []byte{0x00, 0x01, 0x02, 0x03}

	je := newTestEgress(config.WireFormatJPEG, "out.flv")
	if !je.validFrame(frame{data: jpeg}) {
		t.Error("JPEG frame should be valid in jpeg mode")
	}
	if je.validFrame(frame{data: notJPEG}) {
		t.Error("non-JPEG frame should be invalid in jpeg mode")
	}

	re := newTestEgress(config.WireFormatRaw, "out.flv")
	const w, h = 4, 4
	raw := make([]byte, rawFrameSize(w, h))
	if !re.validFrame(frame{data: raw, width: w, height: h}) {
		t.Error("correctly sized raw frame should be valid")
	}
	if re.validFrame(frame{data: raw[:len(raw)-1], width: w, height: h}) {
		t.Error("undersized raw frame should be invalid")
	}
}

func TestEnqueueDropsOldest(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "out.flv")
	// Fill past capacity; queue holds egressQueueSize (5). Oldest must be dropped.
	total := egressQueueSize + 2 // 7 frames -> expect the last 5 retained
	for i := 1; i <= total; i++ {
		e.Enqueue(frame{timestamp: uint32(i)})
	}
	if len(e.input) != egressQueueSize {
		t.Fatalf("queue length = %d, want %d", len(e.input), egressQueueSize)
	}
	var got []uint32
	for len(e.input) > 0 {
		got = append(got, (<-e.input).timestamp)
	}
	want := []uint32{3, 4, 5, 6, 7}
	if len(got) != len(want) {
		t.Fatalf("drained %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("drained %v, want %v (drop-oldest violated)", got, want)
		}
	}
	// Two frames were dropped; the metric must reflect it.
	if n := egressDroppedCount(e.metrics); n != 2 {
		t.Fatalf("egress dropped metric = %d, want 2", n)
	}
}

func TestHandleStderrLineParsesProgress(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "rtmp://host/live2/secretkey")
	e.handleStderrLine("dup_frames=7")
	e.handleStderrLine("drop_frames=4")
	e.handleStderrLine("fps=30.0") // progress but not a counter we store
	out := prometheusDump(e.metrics)
	if !strings.Contains(out, "innolive_egress_dup_frames 7") {
		t.Errorf("dup_frames gauge not set:\n%s", out)
	}
	if !strings.Contains(out, "innolive_egress_drop_frames 4") {
		t.Errorf("drop_frames gauge not set:\n%s", out)
	}
}

func TestHandleStderrLineMasksKeyInErrors(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "rtmp://a.rtmp.youtube.com/live2/secretkey123")
	var buf bytes.Buffer
	e.logger = slog.New(slog.NewTextHandler(&buf, nil))
	e.handleStderrLine("Error opening output rtmp://a.rtmp.youtube.com/live2/secretkey123: Connection refused")
	logged := buf.String()
	if strings.Contains(logged, "secretkey123") {
		t.Errorf("stream key leaked into logs: %s", logged)
	}
	if !strings.Contains(logged, "live2/****") {
		t.Errorf("masked URL not present in logs: %s", logged)
	}
}

func TestEgressStatusTransitions(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "rtmp://a.rtmp.youtube.com/live2/secretkey")

	initial := e.Status()
	if initial.Phase != EgressPhaseIdle {
		t.Fatalf("initial phase = %q, want %q", initial.Phase, EgressPhaseIdle)
	}
	if initial.TargetURL != "rtmp://a.rtmp.youtube.com/live2/****" {
		t.Fatalf("initial TargetURL = %q, want masked URL", initial.TargetURL)
	}
	if initial.StartedAt != nil || initial.StoppedAt != nil {
		t.Fatal("initial StartedAt/StoppedAt must be nil")
	}

	e.setStreaming()
	streaming := e.Status()
	if streaming.Phase != EgressPhaseStreaming || streaming.StartedAt == nil {
		t.Fatalf("after setStreaming: phase=%q started=%v", streaming.Phase, streaming.StartedAt)
	}
	firstStart := *streaming.StartedAt

	e.noteReconnect(io.ErrUnexpectedEOF)
	reconnecting := e.Status()
	if reconnecting.Phase != EgressPhaseReconnecting {
		t.Fatalf("after noteReconnect: phase = %q", reconnecting.Phase)
	}
	if reconnecting.ReconnectAttempts != 1 {
		t.Fatalf("ReconnectAttempts = %d, want 1", reconnecting.ReconnectAttempts)
	}
	if reconnecting.LastError == nil || *reconnecting.LastError != io.ErrUnexpectedEOF.Error() {
		t.Fatalf("LastError = %v, want %q", reconnecting.LastError, io.ErrUnexpectedEOF)
	}

	// 재연결 후 송출 재개: StartedAt은 최초 시각을 보존해야 한다.
	e.setStreaming()
	resumed := e.Status()
	if resumed.StartedAt == nil || !resumed.StartedAt.Equal(firstStart) {
		t.Fatalf("StartedAt changed across reconnect: %v -> %v", firstStart, resumed.StartedAt)
	}

	e.setStopped()
	stopped := e.Status()
	if stopped.Phase != EgressPhaseStopped || stopped.StoppedAt == nil {
		t.Fatalf("after setStopped: phase=%q stopped=%v", stopped.Phase, stopped.StoppedAt)
	}
	firstStop := *stopped.StoppedAt
	e.setStopped()
	if again := e.Status(); !again.StoppedAt.Equal(firstStop) {
		t.Fatal("StoppedAt must not change on repeated setStopped")
	}
}

func TestCollectStartupFramesWithSeed(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "out.flv")
	seed := frame{timestamp: 999, width: 640, height: 360}
	go func() {
		for i := 0; i < egressMeasureFrames-1; i++ {
			e.input <- frame{timestamp: uint32(i), width: 640, height: 360}
		}
	}()
	startup, ok := e.collectStartupFrames(t.Context(), []frame{seed})
	if !ok {
		t.Fatal("collectStartupFrames returned ok=false")
	}
	if len(startup) != egressMeasureFrames {
		t.Fatalf("collected %d frames, want %d", len(startup), egressMeasureFrames)
	}
	if startup[0].timestamp != seed.timestamp {
		t.Fatalf("startup[0].timestamp = %d, want seed %d", startup[0].timestamp, seed.timestamp)
	}
}

func TestCollectStartupFramesCancelled(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "out.flv")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := e.collectStartupFrames(ctx, nil); ok {
		t.Fatal("collectStartupFrames must return ok=false on cancelled context")
	}
}

func TestResolutionChanged(t *testing.T) {
	tests := []struct {
		name          string
		item          frame
		width, height uint16
		want          bool
	}{
		{"same resolution", frame{width: 1280, height: 720}, 1280, 720, false},
		{"width changed", frame{width: 1920, height: 720}, 1280, 720, true},
		{"height changed", frame{width: 1280, height: 1080}, 1280, 720, true},
		{"both changed", frame{width: 1920, height: 1080}, 1280, 720, true},
		{"zero width skips check", frame{width: 0, height: 720}, 1280, 720, false},
		{"zero height skips check", frame{width: 1280, height: 0}, 1280, 720, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolutionChanged(tc.item, tc.width, tc.height); got != tc.want {
				t.Fatalf("resolutionChanged = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRunStopsOnCancelBeforeSpawn: 프레임이 측정 수에 못 미치면 Run은 FFmpeg를
// 스폰하지 않고 대기하다가, 컨텍스트 취소 시 stopped 상태로 종료해야 한다.
// 트랙이 한 번도 도착하지 않은 세션의 teardown 경로다.
func TestRunStopsOnCancelBeforeSpawn(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "out.flv")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	// 측정 수 미만의 프레임만 공급한다: 스폰 없이 수집 단계에 머문다.
	for i := 0; i < 3; i++ {
		e.Enqueue(frame{timestamp: uint32(i), width: 640, height: 360})
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
	if status := e.Status(); status.Phase != EgressPhaseStopped {
		t.Fatalf("phase after Run = %q, want %q", status.Phase, EgressPhaseStopped)
	}
}

// egressDroppedCount extracts the innolive_egress_frames_dropped_total counter.
func egressDroppedCount(r *metrics.Registry) int {
	out := prometheusDump(r)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "innolive_egress_frames_dropped_total ") {
			var n int
			// value is the trailing field
			fields := strings.Fields(line)
			if len(fields) == 2 {
				for _, c := range fields[1] {
					n = n*10 + int(c-'0')
				}
			}
			return n
		}
	}
	return -1
}

func prometheusDump(r *metrics.Registry) string {
	var buf bytes.Buffer
	r.WritePrometheus(&buf)
	return buf.String()
}
