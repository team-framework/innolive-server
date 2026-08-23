package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

func newTestEgress(wire config.WireFormat, url string) *RTMPEgress {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewRTMPEgress("ffmpeg", logger, metrics.New(), TranscoderOptions{WireFormat: wire}, url, nil, false, 0, "", "")
}

type failingWriteCloser struct{ cause error }

func (w failingWriteCloser) Write([]byte) (int, error) { return 0, w.cause }
func (failingWriteCloser) Close() error                { return nil }

type recordingWriteCloser struct {
	mu     sync.Mutex
	writes [][]byte
}

func (w *recordingWriteCloser) Write(value []byte) (int, error) {
	w.mu.Lock()
	w.writes = append(w.writes, append([]byte(nil), value...))
	w.mu.Unlock()
	return len(value), nil
}

func (*recordingWriteCloser) Close() error { return nil }

func (w *recordingWriteCloser) contains(value []byte) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, written := range w.writes {
		if bytes.Equal(written, value) {
			return true
		}
	}
	return false
}

func (w *recordingWriteCloser) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.writes)
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

func TestParseVideoSize(t *testing.T) {
	cases := []struct {
		value         string
		width, height uint16
		ok            bool
	}{
		{"1280x720", 1280, 720, true},
		{" 1920x1080 ", 1920, 1080, true},
		{"", 0, 0, false},
		{"1280", 0, 0, false},
		{"1280x", 0, 0, false},
		{"x720", 0, 0, false},
		{"0x720", 0, 0, false},
		{"-1280x720", 0, 0, false},
		{"hdx720", 0, 0, false},
		{"99999x720", 0, 0, false}, // uint16 범위 밖
	}
	for _, c := range cases {
		width, height, ok := parseVideoSize(c.value)
		if ok != c.ok || width != c.width || height != c.height {
			t.Errorf("parseVideoSize(%q) = (%d, %d, %t), want (%d, %d, %t)",
				c.value, width, height, ok, c.width, c.height, c.ok)
		}
	}
}

// TestVideoFilter는 EGRESS_VIDEO_SIZE 설정 여부에 따른 -vf 체인을 고정한다.
// 고정하지 않았을 때의 체인이 바뀌면 기존 송출 동작이 달라지므로 함께 잠근다.
func TestVideoFilter(t *testing.T) {
	cases := []struct {
		name      string
		wire      config.WireFormat
		videoSize string
		want      string
	}{
		{"jpeg/미고정", config.WireFormatJPEG, "", "scale=out_range=tv"},
		{"raw/미고정", config.WireFormatRaw, "", ""},
		{
			"jpeg/고정", config.WireFormatJPEG, "1280x720",
			"scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:-1:-1,scale=out_range=tv",
		},
		{
			"raw/고정", config.WireFormatRaw, "1280x720",
			"scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:-1:-1",
		},
		{"jpeg/형식 오류는 무시", config.WireFormatJPEG, "hd", "scale=out_range=tv"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEgress(c.wire, "out.flv")
			e.videoSize = c.videoSize
			if got := e.videoFilter(); got != c.want {
				t.Errorf("videoFilter() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestVideoBitrateForPinnedSize는 출력 해상도를 고정했을 때 bitrate가 측정된
// 입력이 아니라 고정 해상도를 기준으로 선택되는지 확인한다. 램프업 저해상도가
// 기준이 되면 FHD로 고정해도 720p용 bitrate가 나간다.
func TestVideoBitrateForPinnedSize(t *testing.T) {
	pinnedFHD := newTestEgress(config.WireFormatJPEG, "out.flv")
	pinnedFHD.videoSize = "1920x1080"
	if got := pinnedFHD.videoBitrateFor(320, 180); got != egressVideoBitrateFHD {
		t.Errorf("videoBitrateFor(320, 180) with 1920x1080 pinned = %q, want %q", got, egressVideoBitrateFHD)
	}

	pinnedHD := newTestEgress(config.WireFormatJPEG, "out.flv")
	pinnedHD.videoSize = "1280x720"
	if got := pinnedHD.videoBitrateFor(1920, 1080); got != egressVideoBitrate {
		t.Errorf("videoBitrateFor(1920, 1080) with 1280x720 pinned = %q, want %q", got, egressVideoBitrate)
	}

	// EGRESS_VIDEO_BITRATE는 해상도 고정보다 우선한다.
	override := newTestEgress(config.WireFormatJPEG, "out.flv")
	override.videoSize = "1920x1080"
	override.bitrateOverride = "3000k"
	if got := override.videoBitrateFor(320, 180); got != "3000k" {
		t.Errorf("videoBitrateFor with override = %q, want %q", got, "3000k")
	}
}

func TestCancellationSlateCacheKeepsOnlyCurrentEgressProfile(t *testing.T) {
	e := newTestEgress(config.WireFormatRaw, "out.flv")

	first, err := e.cancellationSlate(1280, 720)
	if err != nil {
		t.Fatalf("create first cancellation slate: %v", err)
	}
	firstCached := e.slate
	if firstCached == nil {
		t.Fatal("first cancellation slate was not retained")
	}
	if len(first.data) != rawFrameSize(1280, 720) {
		t.Fatalf("first raw slate size = %d, want %d", len(first.data), rawFrameSize(1280, 720))
	}

	if _, err := e.cancellationSlate(1280, 720); err != nil {
		t.Fatalf("reuse cancellation slate: %v", err)
	}
	if e.slate != firstCached {
		t.Fatal("same output profile should reuse the egress slate")
	}

	second, err := e.cancellationSlate(720, 1280)
	if err != nil {
		t.Fatalf("replace cancellation slate: %v", err)
	}
	if e.slate == firstCached {
		t.Fatal("changed output profile should replace the prior egress slate")
	}
	if e.slate.width != 720 || e.slate.height != 1280 {
		t.Fatalf("cached profile = %dx%d, want 720x1280", e.slate.width, e.slate.height)
	}
	if len(second.data) != rawFrameSize(720, 1280) {
		t.Fatalf("second raw slate size = %d, want %d", len(second.data), rawFrameSize(720, 1280))
	}

	e.Stop()
	if e.slate != nil {
		t.Fatal("stopped egress must release its cancellation slate")
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

// TestMeasureFPSSurvivesRampUpGaps는 세션 초반의 프레임 드롭이 프레임률 측정을
// 무너뜨리지 않는지 확인한다. 측정 구간인 시작 프레임 몇 장은 WebRTC 대역폭
// 추정이 가장 낮아 인코더가 프레임을 떨구는 때이고, 측정값은 -r/-fps_mode cfr로
// 세션 내내 고정되므로 낮게 잡히면 이후 정상 프레임을 계속 버린다(#127).
func TestMeasureFPSSurvivesRampUpGaps(t *testing.T) {
	// framesWithGaps는 step 간격으로 프레임을 만들되, gaps에 지정된 위치 앞에는
	// step의 배수만큼 공백을 둔다(인코더가 그만큼 프레임을 떨군 상황).
	framesWithGaps := func(n int, step uint32, gaps map[int]uint32) []frame {
		out := make([]frame, 0, n)
		ts := uint32(90000)
		for i := 0; i < n; i++ {
			if extra, ok := gaps[i]; ok {
				ts += step * extra
			}
			out = append(out, frame{timestamp: ts})
			ts += step
		}
		return out
	}
	const step30 = uint32(videoClockRate / 30)
	const step15 = uint32(videoClockRate / 15)

	tests := []struct {
		name  string
		input []frame
		want  int
	}{
		{"공백 없음", framesWithGaps(30, step30, nil), 30},
		{"1초 공백 1회", framesWithGaps(30, step30, map[int]uint32{2: 30}), 30},
		{"0.5초 공백 2회", framesWithGaps(30, step30, map[int]uint32{2: 15, 5: 15}), 30},
		{"초기 프레임드롭 3회", framesWithGaps(30, step30, map[int]uint32{1: 45, 3: 30, 6: 20}), 30},
		{"심한 램프업 4회", framesWithGaps(30, step30, map[int]uint32{1: 90, 2: 60, 4: 45, 8: 30}), 30},
		// 간격이 고르게 넓은 저프레임 소스는 그대로 잡아야 한다 — 공백과 혼동하면 안 된다.
		{"진짜 15fps 소스", framesWithGaps(30, step15, nil), 15},
		{"진짜 15fps 소스에 공백 1회", framesWithGaps(30, step15, map[int]uint32{4: 10}), 15},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := measureFPS(tc.input); got != tc.want {
				t.Fatalf("measureFPS = %d, want %d", got, tc.want)
			}
		})
	}

	t.Run("순서가 뒤바뀐 프레임이 섞여도 흔들리지 않는다", func(t *testing.T) {
		input := framesWithGaps(30, step30, nil)
		input[10].timestamp, input[11].timestamp = input[11].timestamp, input[10].timestamp
		if got := measureFPS(input); got != 30 {
			t.Fatalf("measureFPS = %d, want 30", got)
		}
	})
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
	if initial.PausedAt != nil {
		t.Fatal("initial PausedAt must be nil")
	}
	if e.Pause() {
		t.Fatal("Pause returned true before streaming")
	}

	e.setStreaming(1280, 720, 30)
	streaming := e.Status()
	if streaming.Phase != EgressPhaseStreaming || streaming.StartedAt == nil {
		t.Fatalf("after setStreaming: phase=%q started=%v", streaming.Phase, streaming.StartedAt)
	}
	if streaming.Width != 1280 || streaming.Height != 720 || streaming.FPS != 30 {
		t.Fatalf("after setStreaming: format=%dx%d@%d, want 1280x720@30", streaming.Width, streaming.Height, streaming.FPS)
	}
	firstStart := *streaming.StartedAt

	e.noteReconnect(io.ErrUnexpectedEOF, 1)
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
	e.setStreaming(1920, 1080, 24)
	resumed := e.Status()
	if resumed.StartedAt == nil || !resumed.StartedAt.Equal(firstStart) {
		t.Fatalf("StartedAt changed across reconnect: %v -> %v", firstStart, resumed.StartedAt)
	}
	if resumed.Width != 1920 || resumed.Height != 1080 || resumed.FPS != 24 {
		t.Fatalf("reconnected format=%dx%d@%d, want 1920x1080@24", resumed.Width, resumed.Height, resumed.FPS)
	}

	if !e.Pause() {
		t.Fatal("Pause returned false for a live egress")
	}
	paused := e.Status()
	if paused.Phase != EgressPhasePaused || paused.PausedAt == nil {
		t.Fatalf("paused status = %+v, want paused phase with PausedAt", paused)
	}
	lastPausedAt := *paused.PausedAt
	if e.Pause() {
		t.Fatal("second Pause returned true")
	}
	if !e.Resume() {
		t.Fatal("Resume returned false for a paused egress")
	}
	if afterResume := e.Status(); afterResume.Phase != EgressPhaseStreaming || afterResume.PausedAt == nil || !afterResume.PausedAt.Equal(lastPausedAt) {
		t.Fatalf("resumed status = %+v, want streaming phase with preserved PausedAt", afterResume)
	}

	e.setStopped()
	stopped := e.Status()
	if stopped.Phase != EgressPhaseStopped || stopped.StoppedAt == nil {
		t.Fatalf("after setStopped: phase=%q stopped=%v", stopped.Phase, stopped.StoppedAt)
	}
	if stopped.PausedAt == nil || !stopped.PausedAt.Equal(lastPausedAt) {
		t.Fatalf("stopped status lost PausedAt: %+v", stopped)
	}
	firstStop := *stopped.StoppedAt
	e.setStopped()
	if again := e.Status(); !again.StoppedAt.Equal(firstStop) {
		t.Fatal("StoppedAt must not change on repeated setStopped")
	}
}

func TestInputRecoveryKeepsCancellationSlateWithoutChangingUserPauseState(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "rtmp://a.rtmp.youtube.com/live2/secretkey")
	e.setStreaming(1280, 720, 30)

	if !e.BeginInputRecovery() {
		t.Fatal("BeginInputRecovery returned false for a streaming egress")
	}
	if status := e.Status(); status.Phase != EgressPhaseStreaming || !status.InputRecovering {
		t.Fatalf("status during input recovery = %+v, want streaming with InputRecovering", status)
	}
	if !e.shouldWriteCancellationSlate() {
		t.Fatal("input recovery must write cancellation slate")
	}
	if !e.EndInputRecovery() {
		t.Fatal("EndInputRecovery returned false")
	}
	if status := e.Status(); status.Phase != EgressPhaseStreaming || status.InputRecovering {
		t.Fatalf("status after input recovery = %+v, want streaming without InputRecovering", status)
	}

	if !e.Pause() {
		t.Fatal("Pause returned false for streaming egress")
	}
	if !e.BeginInputRecovery() {
		t.Fatal("BeginInputRecovery returned false while user-paused")
	}
	if !e.EndInputRecovery() {
		t.Fatal("EndInputRecovery returned false while user-paused")
	}
	if status := e.Status(); status.Phase != EgressPhasePaused || status.InputRecovering {
		t.Fatalf("user pause changed by input recovery = %+v", status)
	}
	if !e.shouldWriteCancellationSlate() {
		t.Fatal("user pause must keep cancellation slate after input recovery")
	}
}

func TestInputRecoveryWritesSlateInsteadOfPendingCameraFrame(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "rtmp://a.rtmp.youtube.com/live2/secretkey")
	e.setStreaming(320, 180, 60)
	if !e.BeginInputRecovery() {
		t.Fatal("BeginInputRecovery returned false")
	}
	slate, err := e.cancellationSlate(320, 180)
	if err != nil {
		t.Fatalf("cancellationSlate: %v", err)
	}
	camera := append([]byte(nil), slate.data...)
	camera[len(camera)/2] ^= 0x01
	if !isJPEG(camera) {
		t.Fatal("test camera frame must remain JPEG-shaped")
	}
	writer := &recordingWriteCloser{}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, _, err = e.writeFrames(ctx, &ffmpegProcess{stdin: writer}, []frame{{data: camera, width: 320, height: 180}}, 320, 180, 60, func() {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("writeFrames error = %v, want context deadline", err)
	}
	if writer.count() == 0 {
		t.Fatal("input recovery did not write any cancellation slate frame")
	}
	if writer.contains(camera) {
		t.Fatal("input recovery wrote a pending camera frame instead of cancellation slate")
	}
}

func TestEgressStopReasonPreservesFirstTerminalCause(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "rtmp://host/live2/secret-key")
	e.setStreaming(1280, 720, 30)
	e.StopWithReason(EgressStopReasonReconnectExhausted)

	stopped := e.Status()
	if stopped.Phase != EgressPhaseStopped {
		t.Fatalf("phase = %q, want stopped", stopped.Phase)
	}
	if stopped.StopReason == nil || *stopped.StopReason != EgressStopReasonReconnectExhausted {
		t.Fatalf("StopReason = %v, want %q", stopped.StopReason, EgressStopReasonReconnectExhausted)
	}

	// 다른 종료 사건이 뒤늦게 와도 최초 종료 사유를 바꾸면 안 된다.
	e.StopWithReason(EgressStopReasonReconnectInputTimeout)
	if status := e.Status(); status.StopReason == nil || *status.StopReason != EgressStopReasonReconnectExhausted {
		t.Fatalf("StopReason after second stop = %v, want first reason", status.StopReason)
	}
}

func TestEgressTerminalMetricsCountOnlyAppliedStop(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "out.flv")
	if !e.StopWithError(EgressStopReasonReconnectExhausted, errors.New("broken pipe")) {
		t.Fatal("first terminal stop must be applied")
	}
	if e.StopWithError(EgressStopReasonReconnectInputTimeout, errReconnectInputTimeout) {
		t.Fatal("second terminal stop must not be applied")
	}

	metricsOutput := prometheusDump(e.metrics)
	for _, expected := range []string{
		"innolive_egress_reconnect_exhausted_total 1",
		"innolive_egress_reconnect_input_timeout_total 0",
	} {
		if !strings.Contains(metricsOutput, expected) {
			t.Fatalf("metrics output does not contain %q\n%s", expected, metricsOutput)
		}
	}
}

func TestEgressStatusMasksOutputURLInLastError(t *testing.T) {
	const outputURL = "rtmp://host/live2/secret-key"
	e := newTestEgress(config.WireFormatJPEG, outputURL)
	e.setStreaming(1280, 720, 30)
	e.noteReconnect(errors.New("write "+outputURL+": broken pipe"), 0)

	status := e.Status()
	if status.LastError == nil {
		t.Fatal("LastError = nil")
	}
	if strings.Contains(*status.LastError, "secret-key") {
		t.Fatalf("stream key leaked in LastError: %q", *status.LastError)
	}
	if !strings.Contains(*status.LastError, "live2/****") {
		t.Fatalf("LastError = %q, want masked URL", *status.LastError)
	}
}

func TestReconnectBudgetBoundsAttemptsAndElapsedTime(t *testing.T) {
	policy := reconnectPolicy{
		maxAttempts: 6,
		maxElapsed:  90 * time.Second,
		minBackoff:  time.Second,
		maxBackoff:  30 * time.Second,
	}
	budget := newReconnectBudget(policy)
	started := time.Date(2026, time.August, 22, 10, 0, 0, 0, time.UTC)
	budget.noteFailure(started)

	wantDelays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}
	now := started
	for attempt, wantDelay := range wantDelays {
		delay, ok := budget.nextDelay(now)
		if !ok || delay != wantDelay {
			t.Fatalf("attempt %d delay = (%s, %t), want (%s, true)", attempt+1, delay, ok, wantDelay)
		}
		now = now.Add(delay)
		if !budget.beginAttempt(now) {
			t.Fatalf("attempt %d was unexpectedly rejected", attempt+1)
		}
	}
	if budget.attempts != len(wantDelays) {
		t.Fatalf("attempts = %d, want %d", budget.attempts, len(wantDelays))
	}
	if _, ok := budget.nextDelay(now); ok {
		t.Fatal("retry must stop after the configured maximum attempts")
	}

	budget.reset()
	if !budget.firstFailed.IsZero() || budget.attempts != 0 {
		t.Fatalf("reset budget = %+v, want empty incident", budget)
	}
}

func TestReconnectBudgetNeverStartsAfterDeadline(t *testing.T) {
	policy := reconnectPolicy{
		maxAttempts: 6,
		maxElapsed:  90 * time.Second,
		minBackoff:  time.Second,
		maxBackoff:  30 * time.Second,
	}
	budget := newReconnectBudget(policy)
	started := time.Date(2026, time.August, 22, 10, 0, 0, 0, time.UTC)
	budget.noteFailure(started)

	// deadline 직전에는 남은 1초만 기다리고, 정확히 deadline에 도달한 뒤에는
	// 새 FFmpeg 프로세스를 시작하지 않는다.
	now := started.Add(89 * time.Second)
	delay, ok := budget.nextDelay(now)
	if !ok || delay != time.Second {
		t.Fatalf("last delay = (%s, %t), want (1s, true)", delay, ok)
	}
	if budget.beginAttempt(now.Add(delay)) {
		t.Fatal("attempt at the elapsed-time deadline must be rejected")
	}
}

// TestEgressTransitionTable는 상태를 바꾸는 모든 사건이 하나의 전이표에
// 묶여 있고, 허용하지 않은 상태 변경이 현재 상태를 통과시키지 않는지 검증한다.
func TestEgressTransitionTable(t *testing.T) {
	tests := []struct {
		name    string
		current EgressPhase
		event   egressTransitionEvent
		want    EgressPhase
		allowed bool
	}{
		{name: "첫 프로필 확정", current: EgressPhaseIdle, event: egressTransitionProfileReady, want: EgressPhaseStreaming, allowed: true},
		{name: "송출 중 일시 중지", current: EgressPhaseStreaming, event: egressTransitionPause, want: EgressPhasePaused, allowed: true},
		{name: "일시 중지 중 재연결", current: EgressPhasePaused, event: egressTransitionReconnect, want: EgressPhasePausedReconnecting, allowed: true},
		{name: "일시 중지 재연결 완료", current: EgressPhasePausedReconnecting, event: egressTransitionProfileReady, want: EgressPhasePaused, allowed: true},
		{name: "송출 중 해상도 변경", current: EgressPhaseStreaming, event: egressTransitionReconfigure, want: EgressPhaseReconfiguring, allowed: true},
		{name: "정지 중 일시 중지 금지", current: EgressPhaseStopped, event: egressTransitionPause, want: EgressPhaseStopped, allowed: false},
		{name: "프로필 전 일시 중지 금지", current: EgressPhaseIdle, event: egressTransitionPause, want: EgressPhaseIdle, allowed: false},
		{name: "재연결 중 재개 금지", current: EgressPhasePausedReconnecting, event: egressTransitionResume, want: EgressPhasePausedReconnecting, allowed: false},
		{name: "모든 생존 상태에서 정지", current: EgressPhasePausedReconnecting, event: egressTransitionStop, want: EgressPhaseStopped, allowed: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, allowed := nextEgressPhase(tc.current, tc.event)
			if got != tc.want || allowed != tc.allowed {
				t.Fatalf("nextEgressPhase(%q, %d) = (%q, %t), want (%q, %t)", tc.current, tc.event, got, allowed, tc.want, tc.allowed)
			}
		})
	}
}

func TestEgressTransitionRejectsIllegalStateWithoutMetadataMutation(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "rtmp://a.rtmp.youtube.com/live2/secretkey")
	before := e.Status()
	if e.transition(egressTransition{event: egressTransitionPause}) {
		t.Fatal("pause transition from idle must be rejected")
	}
	after := e.Status()
	if after != before {
		t.Fatalf("illegal transition mutated status: before=%+v after=%+v", before, after)
	}
}

func TestEgressStatusPreservesPauseAcrossReconfigureAndReconnect(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "rtmp://a.rtmp.youtube.com/live2/secretkey")
	e.setStreaming(1280, 720, 30)
	if !e.Pause() {
		t.Fatal("Pause returned false for a streaming egress")
	}
	pausedAt := *e.Status().PausedAt

	e.beginReconfiguring()
	if status := e.Status(); status.Phase != EgressPhasePausedReconfiguring {
		t.Fatalf("phase during paused reconfigure = %q, want %q", status.Phase, EgressPhasePausedReconfiguring)
	}

	e.noteReconnect(io.ErrUnexpectedEOF, 1)
	if status := e.Status(); status.Phase != EgressPhasePausedReconnecting {
		t.Fatalf("phase during paused reconnect = %q, want %q", status.Phase, EgressPhasePausedReconnecting)
	}
	e.noteReconnect(io.ErrClosedPipe, 2)
	if status := e.Status(); status.Phase != EgressPhasePausedReconnecting {
		t.Fatalf("phase during repeated paused reconnect = %q, want %q", status.Phase, EgressPhasePausedReconnecting)
	}

	e.setStreaming(720, 1280, 60)
	status := e.Status()
	if status.Phase != EgressPhasePaused {
		t.Fatalf("phase after paused reconnect = %q, want %q", status.Phase, EgressPhasePaused)
	}
	if status.Width != 720 || status.Height != 1280 || status.FPS != 60 {
		t.Fatalf("profile after paused reconnect = %dx%d@%d, want 720x1280@60", status.Width, status.Height, status.FPS)
	}
	if status.PausedAt == nil || !status.PausedAt.Equal(pausedAt) {
		t.Fatalf("PausedAt after paused reconnect = %v, want %v", status.PausedAt, pausedAt)
	}
}

func TestEgressStatusReconfiguringReturnsToStreaming(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "rtmp://a.rtmp.youtube.com/live2/secretkey")
	e.setStreaming(1280, 720, 30)
	e.beginReconfiguring()
	if status := e.Status(); status.Phase != EgressPhaseReconfiguring {
		t.Fatalf("phase during reconfigure = %q, want %q", status.Phase, EgressPhaseReconfiguring)
	}
	e.setStreaming(720, 1280, 60)
	status := e.Status()
	if status.Phase != EgressPhaseStreaming {
		t.Fatalf("phase after reconfigure = %q, want %q", status.Phase, EgressPhaseStreaming)
	}
	if status.Width != 720 || status.Height != 1280 || status.FPS != 60 {
		t.Fatalf("profile after reconfigure = %dx%d@%d, want 720x1280@60", status.Width, status.Height, status.FPS)
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
	startup, result := e.collectStartupFrames(t.Context(), []frame{seed}, time.Time{})
	if result != startupFramesReady {
		t.Fatalf("collectStartupFrames result = %d, want ready", result)
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
	if _, result := e.collectStartupFrames(ctx, nil, time.Time{}); result != startupFramesCanceled {
		t.Fatalf("collectStartupFrames result = %d, want canceled", result)
	}
}

func TestCollectStartupFramesTimesOut(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "out.flv")
	deadline := time.Now().Add(10 * time.Millisecond)
	if _, result := e.collectStartupFrames(t.Context(), nil, deadline); result != startupFramesTimedOut {
		t.Fatalf("collectStartupFrames result = %d, want timed out", result)
	}
}

func TestRunStopsAfterReconnectBudgetExhausted(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "rtmp://host/live2/secret-key")
	e.reconnectPolicy = reconnectPolicy{
		maxAttempts: 2,
		maxElapsed:  time.Second,
		minBackoff:  5 * time.Millisecond,
		maxBackoff:  5 * time.Millisecond,
	}

	var starts atomic.Int32
	e.transcoder.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		starts.Add(1)
		return exec.CommandContext(ctx, "__innolive_missing_ffmpeg_for_reconnect_test__")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	go func() {
		for index := 0; index < egressMeasureFrames; index++ {
			e.input <- frame{timestamp: uint32(index * 3000), width: 640, height: 360}
		}
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after reconnect budget exhaustion")
	}

	status := e.Status()
	if status.Phase != EgressPhaseStopped {
		t.Fatalf("phase = %q, want stopped", status.Phase)
	}
	if status.StopReason == nil || *status.StopReason != EgressStopReasonReconnectExhausted {
		t.Fatalf("StopReason = %v, want %q", status.StopReason, EgressStopReasonReconnectExhausted)
	}
	if status.ReconnectAttempts != 2 {
		t.Fatalf("ReconnectAttempts = %d, want 2", status.ReconnectAttempts)
	}
	if got := starts.Load(); got != 3 {
		t.Fatalf("FFmpeg starts = %d, want initial start plus 2 retries", got)
	}
}

func TestRunResetsReconnectStatusAfterStableOutput(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "out.flv")
	e.reconnectPolicy = reconnectPolicy{
		maxAttempts: 2,
		maxElapsed:  time.Second,
		minBackoff:  5 * time.Millisecond,
		maxBackoff:  5 * time.Millisecond,
	}

	var starts atomic.Int32
	e.transcoder.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		if starts.Add(1) == 1 {
			return exec.CommandContext(ctx, "__innolive_missing_ffmpeg_for_stability_test__")
		}
		return exec.CommandContext(ctx, "cat")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	go func() {
		for index := 0; index < egressMeasureFrames; index++ {
			e.input <- frame{timestamp: uint32(index * 3000), width: 640, height: 360}
		}
	}()

	deadline := time.After(time.Second)
	for {
		status := e.Status()
		if status.Phase == EgressPhaseStreaming && status.ReconnectAttempts == 1 && status.LastError != nil {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("egress did not recover from the first failure: %+v", status)
		case <-time.After(time.Millisecond):
		}
	}

	validJPEG := []byte{0xff, 0xd8, 0xff, 0xd9}
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		for index := 0; index < egressStableFrames; index++ {
			e.input <- frame{data: validJPEG, timestamp: uint32((egressMeasureFrames + index) * 3000), width: 640, height: 360}
		}
	}()
	select {
	case <-feedDone:
	case <-time.After(time.Second):
		t.Fatal("stable frame feed did not drain")
	}

	deadline = time.After(time.Second)
	for {
		status := e.Status()
		if status.Phase == EgressPhaseStreaming && status.ReconnectAttempts == 0 && status.LastError == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stable output did not reset reconnect state: %+v", status)
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestRunStopsAfterWriteFailuresExhaustReconnectBudget(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "rtmp://host/live2/secret-key")
	e.reconnectPolicy = reconnectPolicy{
		maxAttempts: 2,
		maxElapsed:  time.Second,
		minBackoff:  5 * time.Millisecond,
		maxBackoff:  5 * time.Millisecond,
	}

	var starts atomic.Int32
	e.startProcess = func(ctx context.Context, _ uint16, _ uint16, _ int) (*ffmpegProcess, error) {
		starts.Add(1)
		command := exec.CommandContext(ctx, "true")
		if err := command.Start(); err != nil {
			return nil, err
		}
		return &ffmpegProcess{
			cmd:     command,
			stdin:   failingWriteCloser{cause: errors.New("injected write failure")},
			metrics: e.metrics,
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	go func() {
		for index := 0; index < egressMeasureFrames; index++ {
			// 시작 프레임은 형식 측정에만 쓰고, helper가 stdin을 닫을 시간을
			// 확보하려고 실제 FFmpeg stdin에는 쓰지 않는다.
			e.input <- frame{timestamp: uint32(index * 3000), width: 640, height: 360}
		}
	}()

	deadline := time.After(time.Second)
	for e.Status().Phase != EgressPhaseStreaming {
		select {
		case <-deadline:
			t.Fatalf("egress did not reach streaming before write failure: %+v", e.Status())
		case <-time.After(time.Millisecond):
		}
	}
	validJPEG := []byte{0xff, 0xd8, 0xff, 0xd9}
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				e.Enqueue(frame{data: validJPEG, width: 640, height: 360})
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not stop after write failure retry budget exhaustion: %+v input=%d", e.Status(), len(e.input))
	}
	<-feedDone

	status := e.Status()
	if status.Phase != EgressPhaseStopped || status.StopReason == nil || *status.StopReason != EgressStopReasonReconnectExhausted {
		t.Fatalf("terminal status = %+v, want reconnect exhaustion", status)
	}
	if status.ReconnectAttempts != 2 {
		t.Fatalf("ReconnectAttempts = %d, want 2", status.ReconnectAttempts)
	}
	if got := starts.Load(); got != 3 {
		t.Fatalf("FFmpeg starts = %d, want initial start plus 2 retries", got)
	}
}

func TestRunStopsWhenReconfigurationInputTimesOut(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "out.flv")
	e.reconnectPolicy = reconnectPolicy{
		maxAttempts: 2,
		maxElapsed:  10 * time.Millisecond,
		minBackoff:  time.Millisecond,
		maxBackoff:  time.Millisecond,
	}
	e.transcoder.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "cat")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	go func() {
		for index := 0; index < egressMeasureFrames; index++ {
			e.input <- frame{timestamp: uint32(index * 3000), width: 640, height: 360}
		}
	}()

	deadline := time.After(time.Second)
	for e.Status().Phase != EgressPhaseStreaming {
		select {
		case <-deadline:
			t.Fatalf("egress did not reach streaming before reconfiguration: %+v", e.Status())
		case <-time.After(time.Millisecond):
		}
	}
	e.input <- frame{timestamp: 90000, width: 1280, height: 720}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after reconfiguration input timeout")
	}
	status := e.Status()
	if status.Phase != EgressPhaseStopped || status.StopReason == nil || *status.StopReason != EgressStopReasonReconnectInputTimeout {
		t.Fatalf("terminal status = %+v, want input timeout", status)
	}
}

func TestPausedReconnectExhaustionAndManualStopRace(t *testing.T) {
	t.Run("paused reconnect exhaustion keeps terminal reason", func(t *testing.T) {
		e := newTestEgress(config.WireFormatJPEG, "out.flv")
		e.setStreaming(1280, 720, 30)
		if !e.Pause() {
			t.Fatal("Pause returned false")
		}
		budget := newReconnectBudget(reconnectPolicy{maxAttempts: 0, maxElapsed: time.Second, minBackoff: time.Millisecond, maxBackoff: time.Millisecond})
		if e.waitForReconnect(t.Context(), &budget, errors.New("broken pipe")) {
			t.Fatal("reconnect must not be allowed when max attempts is zero")
		}
		status := e.Status()
		if status.Phase != EgressPhaseStopped || status.StopReason == nil || *status.StopReason != EgressStopReasonReconnectExhausted {
			t.Fatalf("terminal status = %+v, want paused reconnect exhaustion", status)
		}
	})

	t.Run("manual stop wins during paused reconnect backoff", func(t *testing.T) {
		e := newTestEgress(config.WireFormatJPEG, "out.flv")
		e.setStreaming(1280, 720, 30)
		if !e.Pause() {
			t.Fatal("Pause returned false")
		}
		budget := newReconnectBudget(reconnectPolicy{maxAttempts: 1, maxElapsed: time.Second, minBackoff: 100 * time.Millisecond, maxBackoff: 100 * time.Millisecond})
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan bool, 1)
		go func() { result <- e.waitForReconnect(ctx, &budget, errors.New("broken pipe")) }()

		deadline := time.After(time.Second)
		for e.Status().Phase != EgressPhasePausedReconnecting {
			select {
			case <-deadline:
				t.Fatalf("egress did not enter paused reconnecting: %+v", e.Status())
			case <-time.After(time.Millisecond):
			}
		}
		e.Stop()
		cancel()
		if allowed := <-result; allowed {
			t.Fatal("manual stop must cancel the pending reconnect attempt")
		}
		status := e.Status()
		if status.Phase != EgressPhaseStopped || status.StopReason != nil {
			t.Fatalf("manual stop was overwritten by reconnect failure: %+v", status)
		}
	})
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
