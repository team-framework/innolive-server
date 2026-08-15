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

	if _, err := manager.StopStream(created.ID); err != nil {
		t.Fatal(err)
	}
	created.mu.RLock()
	cleared := created.egressSlot.Load()
	created.mu.RUnlock()
	if cleared != nil {
		t.Fatal("stop must clear the egress slot")
	}
	stream = created.Response().Stream
	if stream.StopReason == nil || *stream.StopReason != "user_requested" {
		t.Fatalf("StopReason = %v, want user_requested", stream.StopReason)
	}
	if _, err := manager.StopStream(created.ID); !errors.Is(err, ErrStreamNotActive) {
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
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if activeEgress.Status().Phase == media.EgressPhaseStopped {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("egress did not stop after session delete")
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
