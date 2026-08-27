package session

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/media"
	"inno-live-server/internal/metrics"

	"github.com/google/uuid"
)

func newTestManager(t *testing.T, maxSessions int) *Manager {
	t.Helper()
	cfg := config.Config{
		PrivacyMode:    config.PrivacyModeBypass,
		FFmpegPath:     "ffmpeg",
		UDPPortMin:     42000,
		UDPPortMax:     42100,
		FrameQueueSize: 2,
		MaxSessions:    maxSessions,
	}
	manager, err := NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	return manager
}

func TestCreateEnforcesMaxSessions(t *testing.T) {
	manager := newTestManager(t, 2)
	first, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Create(nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Create(nil); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("third Create() error = %v, want ErrCapacityExceeded", err)
	}
	if active, limit := manager.Capacity(); active != 2 || limit != 2 {
		t.Fatalf("Capacity() = (%d, %d), want (2, 2)", active, limit)
	}
	if first.Response().Media.AIFallbackActive {
		t.Fatal("AIFallbackActive should default to false before any track exists")
	}

	if err := manager.Delete(first.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Create(nil); err != nil {
		t.Fatalf("Create() after Delete() error = %v, want slot freed", err)
	}
}

func TestCreateUnlimitedWhenMaxSessionsZero(t *testing.T) {
	manager := newTestManager(t, 0)
	for i := 0; i < 3; i++ {
		if _, _, err := manager.Create(nil); err != nil {
			t.Fatalf("Create() #%d error = %v", i, err)
		}
	}
}

func TestCloseUserSessionsForLogoutClosesOnlyLoggedOutUser(t *testing.T) {
	manager := newTestManager(t, 0)
	loggedOutUserID := uuid.New()
	otherUserID := uuid.New()
	first, _, err := manager.CreateForUser(loggedOutUserID, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := manager.CreateForUser(loggedOutUserID, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := manager.CreateForUser(otherUserID, nil)
	if err != nil {
		t.Fatal(err)
	}

	manager.CloseUserSessionsForLogout(loggedOutUserID)
	for _, id := range []string{first.ID, second.ID} {
		if _, err := manager.Get(id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("logged-out session %s still exists: %v", id, err)
		}
	}
	if _, err := manager.Get(other.ID); err != nil {
		t.Fatalf("other user's session must remain: %v", err)
	}
}

func TestConcurrentCreateNeverExceedsLimit(t *testing.T) {
	const limit = 4
	const attempts = 16
	manager := newTestManager(t, limit)

	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	rejected := 0
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := manager.Create(nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, ErrCapacityExceeded):
				rejected++
			default:
				t.Errorf("Create() unexpected error = %v", err)
			}
		}()
	}
	wg.Wait()

	if created != limit {
		t.Fatalf("created = %d, want exactly %d", created, limit)
	}
	if rejected != attempts-limit {
		t.Fatalf("rejected = %d, want %d", rejected, attempts-limit)
	}
	if sessions := manager.List(); len(sessions) != limit {
		t.Fatalf("List() length = %d, want %d", len(sessions), limit)
	}
}

func TestGetAndDeleteNotFound(t *testing.T) {
	manager := newTestManager(t, 0)
	if _, err := manager.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
	if err := manager.Delete("missing", "test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete() error = %v, want ErrNotFound", err)
	}
}

func TestReapsUnnegotiatedSessions(t *testing.T) {
	cfg := config.Config{
		PrivacyMode:        config.PrivacyModeBypass,
		FFmpegPath:         "ffmpeg",
		UDPPortMin:         42000,
		UDPPortMax:         42100,
		FrameQueueSize:     2,
		NegotiationTimeout: 100 * time.Millisecond,
	}
	manager, err := NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)

	created, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := manager.Get(created.ID); errors.Is(err, ErrNotFound) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("unnegotiated session was not reaped within the timeout window")
}

// TestStartStreamRequiresVideoTrack: 트랙 먼저, 시작 나중 — 501 스텁 시절부터
// 이어지는 409 계약이다(#83).
func TestStartStreamRequiresVideoTrack(t *testing.T) {
	manager := newTestManager(t, 0)
	created, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartStream(created.ID, "rtmps://ingest.example/live2/secret-key"); !errors.Is(err, ErrNoVideoTrack) {
		t.Fatalf("StartStream error = %v, want ErrNoVideoTrack", err)
	}
	if _, err := manager.StartStream("missing", "rtmps://x/y"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session error = %v, want ErrNotFound", err)
	}
}

// TestStartStopStreamLifecycle: 명시적 start~stop 수명(#83) — 시작 시 슬롯에
// 꽂히고, 응답 StreamState가 반영되며(키 비노출), 중지 시 StopReason이 남고,
// 재시작이 가능해야 한다.
func TestStartStopStreamLifecycle(t *testing.T) {
	manager := newTestManager(t, 0)
	created, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	// 트랙 도착을 모사한다(OnTrack 없이 게이트만 통과시키는 최소 설정).
	created.mu.Lock()
	created.rawTrackID = "video-track"
	created.mu.Unlock()

	stream := created.Response().Stream
	if stream.Status != "idle" || stream.TargetURL != nil {
		t.Fatalf("pre-start Stream = %+v, want default idle", stream)
	}

	if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/secret-key"); err != nil {
		t.Fatal(err)
	}
	created.mu.RLock()
	slotted := created.egressSlot.Load()
	active := created.egress
	created.mu.RUnlock()
	if active == nil || slotted != active {
		t.Fatal("started egress must be installed in the session slot")
	}
	stream = created.Response().Stream
	if stream.TargetURL == nil {
		t.Fatal("Stream.TargetURL must be populated after start")
	}
	if strings.Contains(*stream.TargetURL, "secret-key") {
		t.Fatalf("Stream.TargetURL leaks the stream key: %q", *stream.TargetURL)
	}
	if !strings.HasSuffix(*stream.TargetURL, "/****") {
		t.Fatalf("Stream.TargetURL is not masked: %q", *stream.TargetURL)
	}
	if stream.StopReason != nil {
		t.Fatalf("StopReason = %v before stop", *stream.StopReason)
	}

	if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/other"); !errors.Is(err, ErrStreamActive) {
		t.Fatalf("double start error = %v, want ErrStreamActive", err)
	}

	// 프레임을 실제로 공급하지 않는 이 단위 테스트의 egress는 idle 상태다.
	// 취소 슬레이트의 출력 형식이 아직 확정되지 않았으므로 pause를 금지한다.
	if _, err := manager.PauseStream(created.ID); !errors.Is(err, ErrStreamNotActive) {
		t.Fatalf("pause before streaming error = %v, want ErrStreamNotActive", err)
	}

	if _, _, _, err := manager.StopStream(created.ID); err != nil {
		t.Fatal(err)
	}
	created.mu.RLock()
	cleared := created.egressSlot.Load()
	created.mu.RUnlock()
	if cleared != nil {
		t.Fatal("stop must clear the egress slot")
	}
	stream = created.Response().Stream
	if stream.Status != string(media.EgressPhaseStopped) {
		t.Fatalf("Stream.Status after stop = %q, want %q", stream.Status, media.EgressPhaseStopped)
	}
	if stream.StopReason == nil || *stream.StopReason != "user_requested" {
		t.Fatalf("StopReason = %v, want user_requested", stream.StopReason)
	}
	if _, _, _, err := manager.StopStream(created.ID); !errors.Is(err, ErrStreamNotActive) {
		t.Fatalf("double stop error = %v, want ErrStreamNotActive", err)
	}
	// 중지된 egress는 종료 타이밍과 무관하게 재시작을 막지 않아야 한다.
	if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/second-key"); err != nil {
		t.Fatalf("restart after stop failed: %v", err)
	}

	// 세션 삭제가 활성 egress를 정리해야 한다.
	restarted := created
	restarted.mu.RLock()
	activeEgress := restarted.egress
	restarted.mu.RUnlock()
	if err := manager.Delete(created.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if activeEgress.Status().Phase != media.EgressPhaseStopped {
		t.Fatalf("egress phase after session delete = %q, want %q", activeEgress.Status().Phase, media.EgressPhaseStopped)
	}
}

func TestTerminalEgressClearsSlotAndAllowsRestart(t *testing.T) {
	manager := newTestManager(t, 0)
	created, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	created.mu.Lock()
	created.rawTrackID = "video-track"
	created.mu.Unlock()

	if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/secret-key"); err != nil {
		t.Fatal(err)
	}
	created.mu.RLock()
	egress := created.egress
	cancel := created.egressCancel
	created.mu.RUnlock()
	if egress == nil || cancel == nil {
		t.Fatal("StartStream must install an egress and cancellation function")
	}

	terminal := errors.New("RTMP write failed for rtmps://a.rtmps.youtube.com/live2/secret-key")
	egress.StopWithError(media.EgressStopReasonReconnectExhausted, terminal)
	cancel()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		created.mu.RLock()
		slotted := created.egressSlot.Load()
		created.mu.RUnlock()
		if slotted == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	created.mu.RLock()
	slotted := created.egressSlot.Load()
	created.mu.RUnlock()
	if slotted != nil {
		t.Fatal("terminal egress must be removed from the egress slot")
	}

	stream := created.Response().Stream
	if stream.Status != string(media.EgressPhaseStopped) {
		t.Fatalf("Stream.Status = %q, want stopped", stream.Status)
	}
	if stream.StopReason == nil || *stream.StopReason != string(media.EgressStopReasonReconnectExhausted) {
		t.Fatalf("Stream.StopReason = %v, want reconnect exhaustion", stream.StopReason)
	}
	if stream.LastError == nil || strings.Contains(*stream.LastError, "secret-key") {
		t.Fatalf("Stream.LastError = %v, want sanitized terminal error", stream.LastError)
	}

	if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/restarted-key"); err != nil {
		t.Fatalf("StartStream after terminal egress error = %v", err)
	}
}

type pauseControllerStub struct {
	statuses     []media.EgressStatus
	statusCalls  int
	pauseResult  bool
	resumeResult bool
	pauseCalls   int
	resumeCalls  int
}

func (s *pauseControllerStub) Status() media.EgressStatus {
	index := s.statusCalls
	s.statusCalls++
	if index >= len(s.statuses) {
		return s.statuses[len(s.statuses)-1]
	}
	return s.statuses[index]
}

func (s *pauseControllerStub) Pause() bool {
	s.pauseCalls++
	return s.pauseResult
}

func (s *pauseControllerStub) Resume() bool {
	s.resumeCalls++
	return s.resumeResult
}

// TestPauseResumeEgressStoppedDuringControl은 첫 상태 확인 뒤 egress가
// 종료되는 경합에서 "이미 일시 중지"나 "일시 중지 상태 아님"이 아니라
// "활성 송출 없음"으로 분류돼야 함을 검증한다.
func TestPauseResumeEgressStoppedDuringControl(t *testing.T) {
	for _, tc := range []struct {
		name         string
		initialPhase media.EgressPhase
		call         func(streamPauseController) error
	}{
		{name: "pause", initialPhase: media.EgressPhaseStreaming, call: pauseEgress},
		{name: "resume", initialPhase: media.EgressPhasePaused, call: resumeEgress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			egress := &pauseControllerStub{
				statuses: []media.EgressStatus{
					{Phase: tc.initialPhase},
					{Phase: media.EgressPhaseStopped},
				},
			}
			if err := tc.call(egress); !errors.Is(err, ErrStreamNotActive) {
				t.Fatalf("error = %v, want ErrStreamNotActive", err)
			}
		})
	}
}

// TestPauseResumeEgressStateContract는 API 제어 전 상태가 허용된 Enum 전이만
// 수행하는지 검증한다. 특히 첫 프레임 수집 중인 idle egress는 pause 대상이 아니다.
func TestPauseResumeEgressStateContract(t *testing.T) {
	tests := []struct {
		name            string
		call            func(streamPauseController) error
		phase           media.EgressPhase
		pauseResult     bool
		resumeResult    bool
		want            error
		wantPauseCalls  int
		wantResumeCalls int
	}{
		{
			name:           "pause streaming",
			call:           pauseEgress,
			phase:          media.EgressPhaseStreaming,
			pauseResult:    true,
			wantPauseCalls: 1,
		},
		{
			name:  "pause idle is not active",
			call:  pauseEgress,
			phase: media.EgressPhaseIdle,
			want:  ErrStreamNotActive,
		},
		{
			name:  "pause already paused",
			call:  pauseEgress,
			phase: media.EgressPhasePaused,
			want:  ErrStreamPaused,
		},
		{
			name:  "pause while paused reconnecting is already paused",
			call:  pauseEgress,
			phase: media.EgressPhasePausedReconnecting,
			want:  ErrStreamPaused,
		},
		{
			name:            "resume paused",
			call:            resumeEgress,
			phase:           media.EgressPhasePaused,
			resumeResult:    true,
			wantResumeCalls: 1,
		},
		{
			name:  "resume streaming is not paused",
			call:  resumeEgress,
			phase: media.EgressPhaseStreaming,
			want:  ErrStreamNotPaused,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			egress := &pauseControllerStub{
				statuses:     []media.EgressStatus{{Phase: tc.phase}},
				pauseResult:  tc.pauseResult,
				resumeResult: tc.resumeResult,
			}
			err := tc.call(egress)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if egress.pauseCalls != tc.wantPauseCalls {
				t.Fatalf("Pause calls = %d, want %d", egress.pauseCalls, tc.wantPauseCalls)
			}
			if egress.resumeCalls != tc.wantResumeCalls {
				t.Fatalf("Resume calls = %d, want %d", egress.resumeCalls, tc.wantResumeCalls)
			}
		})
	}
}

func TestStreamStateUsesEgressPhaseAsStatus(t *testing.T) {
	pausedAt := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	state := streamStateFromEgress(media.EgressStatus{
		Phase:    media.EgressPhasePaused,
		PausedAt: &pausedAt,
	}, true, nil)
	if state.Status != string(media.EgressPhasePaused) {
		t.Fatalf("status = %q, want %q", state.Status, media.EgressPhasePaused)
	}
	if state.PausedAt == nil || !state.PausedAt.Equal(pausedAt) {
		t.Fatalf("PausedAt = %v, want %v", state.PausedAt, pausedAt)
	}
}

func TestStreamStatePrefersSessionStopReason(t *testing.T) {
	egressReason := media.EgressStopReasonReconnectExhausted
	sessionReason := "user_requested"
	state := streamStateFromEgress(media.EgressStatus{
		Phase:      media.EgressPhaseStopped,
		StopReason: &egressReason,
	}, true, &sessionReason)
	if state.StopReason == nil || *state.StopReason != sessionReason {
		t.Fatalf("StopReason = %v, want session reason %q", state.StopReason, sessionReason)
	}
}

// newReapTestManager는 회수 타임아웃을 짧게 준 매니저다.
func newReapTestManager(t *testing.T, timeout time.Duration) *Manager {
	t.Helper()
	cfg := config.Config{
		PrivacyMode:        config.PrivacyModeBypass,
		FFmpegPath:         "ffmpeg",
		UDPPortMin:         42000,
		UDPPortMax:         42100,
		FrameQueueSize:     2,
		NegotiationTimeout: timeout,
	}
	manager, err := NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	return manager
}

// waitUntilReaped는 세션이 회수될 때까지 기다리고, since부터 회수까지 걸린
// 시간을 돌려준다. 회수 기한은 생성·마지막 활동 시점부터 재므로 기준 시각을
// 호출자가 준다 — 이 함수가 호출된 시점부터 재면 그 사이 간격만큼 늘 짧게
// 측정되어, -race처럼 느린 환경에서 경계값 비교가 헛되이 실패한다.
func waitUntilReaped(t *testing.T, manager *Manager, id string, since time.Time, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < within {
		if _, err := manager.Get(id); errors.Is(err, ErrNotFound) {
			return time.Since(since)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("session %s was not reaped within %s", id, within)
	return 0
}

// 연장에는 상한이 있다(#147). 방송 설정 저장을 계속해도 생성 후
// maxNegotiationExtension이 지나면 협상 없는 세션은 회수된다.
func TestReapUnnegotiatedStopsExtendingAfterLimit(t *testing.T) {
	const timeout = 50 * time.Millisecond
	original := maxNegotiationExtension
	maxNegotiationExtension = 120 * time.Millisecond
	t.Cleanup(func() { maxNegotiationExtension = original })

	manager := newReapTestManager(t, timeout)

	// 연장 상한은 세션 생성 시점부터 잰다.
	createdAt := time.Now()
	s, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}

	// 상한을 넘길 때까지 저장을 계속해도 회수를 막지 못한다.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(timeout / 2):
				_, _ = manager.SetBroadcastSettings(s.ID, YouTubeBroadcastSettings{Title: "작성 중"})
			}
		}
	}()

	if elapsed := waitUntilReaped(t, manager, s.ID, createdAt, 2*time.Second); elapsed < maxNegotiationExtension {
		t.Fatalf("session reaped after %s, want at least the extension limit %s", elapsed, maxNegotiationExtension)
	}
}

// 방송 설정을 저장하는 동안에는 회수되지 않는다(#147). 저장은 방치가 아니므로
// 타임아웃을 마지막 저장 시점부터 다시 재야 한다.
func TestReapUnnegotiatedDefersWhileBroadcastSettingsUpdated(t *testing.T) {
	const timeout = 100 * time.Millisecond
	manager := newReapTestManager(t, timeout)

	s, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}

	// 원래 회수 시점을 넘길 때까지 설정을 저장하며 버틴다.
	lastActivity := time.Now()
	for i := 0; i < 3; i++ {
		time.Sleep(timeout / 2)
		if _, err := manager.SetBroadcastSettings(s.ID, YouTubeBroadcastSettings{Title: "작성 중"}); err != nil {
			t.Fatalf("SetBroadcastSettings after %s: %v", time.Duration(i+1)*timeout/2, err)
		}
		lastActivity = time.Now()
	}

	// 저장을 멈추면 마지막 저장 이후 timeout이 지나 회수된다.
	if elapsed := waitUntilReaped(t, manager, s.ID, lastActivity, 2*time.Second); elapsed < timeout {
		t.Fatalf("session reaped after %s, want at least %s since last activity", elapsed, timeout)
	}
}
