package session

import (
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/media"
	"inno-live-server/internal/metrics"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
)

type recordedUsage struct {
	mu     sync.Mutex
	events []UsageEvent
}

func (r *recordedUsage) RecordUsage(event UsageEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordedUsage) kinds() []UsageEventKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	kinds := make([]UsageEventKind, 0, len(r.events))
	for _, event := range r.events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

type recordedCleanups struct {
	mu         sync.Mutex
	broadcasts []PlatformBroadcast
}

func (r *recordedCleanups) record(_ uuid.UUID, broadcast PlatformBroadcast, _ BroadcastPhase) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.broadcasts = append(r.broadcasts, broadcast)
}

func (r *recordedCleanups) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.broadcasts)
}

func newHandoffTestManager(t *testing.T, recoveryWindow time.Duration) (*Manager, *recordedUsage, *recordedCleanups) {
	t.Helper()
	manager, err := NewManager(config.Config{
		PrivacyMode:          config.PrivacyModeBypass,
		FFmpegPath:           "ffmpeg",
		UDPPortMin:           42000,
		UDPPortMax:           42100,
		FrameQueueSize:       2,
		WebRTCRecoveryWindow: recoveryWindow,
		RequireSessionAuth:   true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	usage := &recordedUsage{}
	cleanups := &recordedCleanups{}
	manager.SetUsageRecorder(usage)
	manager.SetBroadcastCleanup(cleanups.record)
	return manager, usage, cleanups
}

// liveSessionForHandoff는 유튜브 방송이 라이브인 회원 세션을 만든다.
func liveSessionForHandoff(t *testing.T, manager *Manager) (*Session, string) {
	t.Helper()
	s, token, err := manager.CreateForUser(uuid.New(), map[string]string{"client": "ios"})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	t0 := s.target(s.Provider)
	t0.phase = BroadcastPhaseLive
	t0.platformBroadcast = &PlatformBroadcast{Provider: s.Provider, BroadcastID: "yt-broadcast", StreamID: "yt-stream"}
	t0.channel = "channel-1"
	s.anonymizationEnabled = false
	s.screenBlurEnabled = true
	s.mu.Unlock()
	return s, token
}

func newClientOffer(t *testing.T) string {
	t.Helper()
	client, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	return offer.SDP
}

// 넘겨받은 세션은 같은 소유자 토큰과 설정·방송 상태로 이어지고, 옛 프로세스는 플랫폼
// 방송을 끝내거나 세션 종료를 기록하지 않고 놓는다(#517).
func TestHandoffMovesSessionWithoutEndingBroadcast(t *testing.T) {
	old, oldUsage, oldCleanups := newHandoffTestManager(t, time.Minute)
	fresh, freshUsage, _ := newHandoffTestManager(t, time.Minute)
	s, token := liveSessionForHandoff(t, old)

	snapshot, err := old.ExportSession(s.ID)
	if err != nil {
		t.Fatalf("ExportSession() error = %v", err)
	}
	if err := fresh.ImportSession(snapshot, false); err != nil {
		t.Fatalf("ImportSession() error = %v", err)
	}
	if err := old.DetachSession(s.ID); err != nil {
		t.Fatalf("DetachSession() error = %v", err)
	}
	if err := fresh.CommitHandoff(s.ID); err != nil {
		t.Fatalf("CommitHandoff() error = %v", err)
	}

	if _, err := old.Get(s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old Get() error = %v, want ErrNotFound", err)
	}
	if oldCleanups.count() != 0 {
		t.Fatalf("old process ended %d platform broadcasts during handoff", oldCleanups.count())
	}
	for _, kind := range oldUsage.kinds() {
		if kind == UsageSessionEnded {
			t.Fatalf("old process recorded %v during handoff", kind)
		}
	}
	for _, kind := range freshUsage.kinds() {
		if kind == UsageSessionStarted {
			t.Fatalf("new process recorded %v for a handed-off session", kind)
		}
	}

	imported, err := fresh.VerifyOwner(s.ID, token)
	if err != nil {
		t.Fatalf("VerifyOwner() on new process error = %v", err)
	}
	broadcast, phase := imported.PlatformBroadcast()
	if phase != BroadcastPhaseLive || broadcast.BroadcastID != "yt-broadcast" {
		t.Fatalf("imported broadcast = %+v (%s), want live yt-broadcast", broadcast, phase)
	}
	imported.mu.RLock()
	settings := [2]bool{imported.anonymizationEnabled, imported.screenBlurEnabled}
	recovering := imported.recovery.status == PeerRecoveryStatusRecovering
	imported.mu.RUnlock()
	if settings != [2]bool{false, true} || !recovering {
		t.Fatalf("imported settings = %v recovering = %v", settings, recovering)
	}
	if response := imported.Response(); response.SessionID != s.ID {
		t.Fatalf("imported Response().SessionID = %q", response.SessionID)
	}
}

// 넘겨받은 세션은 클라이언트의 새 PeerConnection(iOS 일반 offer)이든 ICE restart
// offer(Android)든 첫 협상으로 받는다.
func TestImportedSessionAcceptsPlainAndRestartOffers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options NegotiationOptions
	}{
		{"plain offer", NegotiationOptions{}},
		{"ice restart offer", NegotiationOptions{ICERestart: true, NegotiationID: "negotiation-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, _, _ := newHandoffTestManager(t, time.Minute)
			fresh, _, _ := newHandoffTestManager(t, time.Minute)
			s, token := liveSessionForHandoff(t, old)
			snapshot, err := old.ExportSession(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := fresh.ImportSession(snapshot, true); err != nil {
				t.Fatal(err)
			}

			answer, err := fresh.CreateAnswerWithOptions(s.ID, token, newClientOffer(t), tc.options)
			if err != nil {
				t.Fatalf("CreateAnswerWithOptions() error = %v", err)
			}
			if answer.Type != "answer" || answer.NegotiationID != tc.options.NegotiationID {
				t.Fatalf("answer = %+v", answer)
			}
			if _, err := fresh.CreateAnswer(s.ID, "wrong-token", newClientOffer(t)); err == nil {
				t.Fatal("CreateAnswer() accepted a wrong owner token")
			}
		})
	}
}

// 클라이언트가 아직 붙지 않은 세션은 최신 스냅샷으로 바꾸고, 이미 붙은 세션은 덮어쓰지
// 않는다.
func TestImportReplacesPlaceholderButNotReconnectedSession(t *testing.T) {
	old, _, _ := newHandoffTestManager(t, time.Minute)
	fresh, _, _ := newHandoffTestManager(t, time.Minute)
	s, token := liveSessionForHandoff(t, old)
	first, err := old.ExportSession(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.ImportSession(first, false); err != nil {
		t.Fatal(err)
	}

	latest := first
	latest.ScreenBlurEnabled = false
	if err := fresh.ImportSession(latest, true); err != nil {
		t.Fatalf("ImportSession(latest) error = %v", err)
	}
	imported, err := fresh.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	imported.mu.RLock()
	screenBlur := imported.screenBlurEnabled
	imported.mu.RUnlock()
	if screenBlur {
		t.Fatal("placeholder was not replaced by the latest snapshot")
	}

	if _, err := fresh.CreateAnswer(s.ID, token, newClientOffer(t)); err != nil {
		t.Fatal(err)
	}
	if err := fresh.ImportSession(first, false); !errors.Is(err, ErrHandoffConflict) {
		t.Fatalf("ImportSession() over a reconnected session error = %v, want ErrHandoffConflict", err)
	}
	if err := fresh.ImportSession(first, true); err != nil {
		t.Fatalf("committing ImportSession() over a reconnected handed-off session error = %v", err)
	}
}

// 플랫폼 왕복 중이거나 게스트인 세션은 넘기지 않는다.
func TestExportRejectsBusyAndGuestSessions(t *testing.T) {
	manager, _, _ := newHandoffTestManager(t, time.Minute)
	s, _ := liveSessionForHandoff(t, manager)
	s.mu.Lock()
	s.target(s.Provider).phase = BroadcastPhaseGoingLive
	s.mu.Unlock()
	if _, err := manager.ExportSession(s.ID); !errors.Is(err, ErrHandoffBusy) {
		t.Fatalf("ExportSession(going live) error = %v, want ErrHandoffBusy", err)
	}

	guest, _, err := manager.CreateForGuest("guest-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ExportSession(guest.ID); !errors.Is(err, ErrHandoffUnsupported) {
		t.Fatalf("ExportSession(guest) error = %v, want ErrHandoffUnsupported", err)
	}
}

// 클라이언트가 복구 창 안에 다시 붙지 않으면 평소의 복구 소진처럼 정리한다.
func TestImportedSessionExpiresWithoutReconnect(t *testing.T) {
	old, _, _ := newHandoffTestManager(t, time.Minute)
	fresh, _, freshCleanups := newHandoffTestManager(t, 100*time.Millisecond)
	s, _ := liveSessionForHandoff(t, old)
	snapshot, err := old.ExportSession(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.ImportSession(snapshot, true); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := fresh.Get(s.ID); errors.Is(err, ErrNotFound) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := fresh.Get(s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("imported session survived past the recovery window")
	}
	deadline = time.Now().Add(time.Second)
	for freshCleanups.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if freshCleanups.count() != 1 {
		t.Fatalf("expired session ended %d platform broadcasts, want 1", freshCleanups.count())
	}
}

// 송출은 옛 프로세스가 놓고 클라이언트 영상이 다시 들어온 뒤에만 다시 붙인다. 그 전에
// 붙이면 같은 스트림 키에 두 연결이 겹친다.
func TestHandoffEgressWaitsForCommitAndVideo(t *testing.T) {
	manager, _, _ := newHandoffTestManager(t, time.Minute)
	s, _ := liveSessionForHandoff(t, manager)
	snapshot, err := manager.ExportSession(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ID = uuid.NewString()
	snapshot.Targets[0].Egress = &EgressSnapshot{OutputURL: "rtmp://example.invalid/live/key"}
	fresh, _, _ := newHandoffTestManager(t, time.Minute)
	if err := fresh.ImportSession(snapshot, false); err != nil {
		t.Fatal(err)
	}
	imported, err := fresh.Get(snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}

	pending := func() int {
		imported.mu.RLock()
		defer imported.mu.RUnlock()
		return len(imported.handoff.egress)
	}
	fresh.resumeHandoffEgress(imported)
	if pending() != 1 {
		t.Fatal("egress resumed before the old process let go")
	}
	if err := fresh.CommitHandoff(snapshot.ID); err != nil {
		t.Fatal(err)
	}
	if pending() != 1 {
		t.Fatal("egress resumed before client video arrived")
	}
}

func (r *recordedUsage) kindsFor(broadcastID string) []UsageEventKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	var kinds []UsageEventKind
	for _, event := range r.events {
		if event.BroadcastID == broadcastID {
			kinds = append(kinds, event.Kind)
		}
	}
	return kinds
}

// 넘겨받은 송출은 옛 프로세스의 일시 중지를 그대로 이어 시작하고, 라이브 대상의 새
// 송출 세대는 라이브 시각과 일시 중지를 함께 기록한다(#526).
func TestHandoffResumesEgressWithPauseAndLiveUsage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		paused    bool
		wantPhase media.EgressPhase
		wantKinds []UsageEventKind
	}{
		{"일시 중지 송출", true, media.EgressPhasePausedReconfiguring, []UsageEventKind{UsageBroadcastStarted, UsageBroadcastLive, UsageBroadcastPaused}},
		{"송출 중", false, media.EgressPhaseIdle, []UsageEventKind{UsageBroadcastStarted, UsageBroadcastLive}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, _, _ := newHandoffTestManager(t, time.Minute)
			s, _ := liveSessionForHandoff(t, old)
			snapshot, err := old.ExportSession(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.Targets[0].Egress = &EgressSnapshot{OutputURL: "rtmp://example.invalid/live/key", Paused: tc.paused}
			fresh, freshUsage, _ := newHandoffTestManager(t, time.Minute)
			if err := fresh.ImportSession(snapshot, true); err != nil {
				t.Fatal(err)
			}
			imported, err := fresh.Get(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			imported.mu.Lock()
			imported.rawTrackID = "video"
			imported.mu.Unlock()

			fresh.resumeHandoffEgress(imported)

			imported.mu.RLock()
			target := imported.targets[imported.Provider]
			phase := target.egress.Status().Phase
			usageID := target.usageID
			aiInputPaused := imported.aiInputPaused
			imported.mu.RUnlock()
			if phase != tc.wantPhase {
				t.Fatalf("resumed egress phase = %q, want %q", phase, tc.wantPhase)
			}
			// 첫 프로필을 측정하려면 처리 프레임이 필요하므로 AI 입력은 막지 않는다.
			if aiInputPaused {
				t.Fatal("AI input was suspended before the resumed egress measured its profile")
			}
			if got := freshUsage.kindsFor(usageID); !slices.Equal(got, tc.wantKinds) {
				t.Fatalf("usage events for resumed egress = %v, want %v", got, tc.wantKinds)
			}
		})
	}
}

// 옛 프로세스가 놓기 전의 넘겨받은 세션은 버려도(배포 롤백·commit 실패) 플랫폼 방송을
// 끝내거나 세션 종료를 기록하지 않는다. 그 방송은 옛 프로세스에서 송출 중이다(#526).
func TestUncommittedHandoffDropKeepsPlatformBroadcast(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop func(*Manager, string)
	}{
		{"복구 창 만료", func(m *Manager, id string) { m.expireRecovery(id, 1) }},
		{"프로세스 종료", func(m *Manager, _ string) { m.CloseAll() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, _, _ := newHandoffTestManager(t, time.Minute)
			s, _ := liveSessionForHandoff(t, old)
			snapshot, err := old.ExportSession(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			fresh, freshUsage, freshCleanups := newHandoffTestManager(t, time.Minute)
			if err := fresh.ImportSession(snapshot, false); err != nil {
				t.Fatal(err)
			}

			tc.drop(fresh, s.ID)

			if _, err := fresh.Get(s.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get() after drop error = %v, want ErrNotFound", err)
			}
			time.Sleep(50 * time.Millisecond)
			if freshCleanups.count() != 0 {
				t.Fatalf("dropping an uncommitted handoff ended %d platform broadcasts", freshCleanups.count())
			}
			for _, kind := range freshUsage.kinds() {
				if kind == UsageSessionEnded || kind == UsageBroadcastEnded {
					t.Fatalf("dropping an uncommitted handoff recorded %v", kind)
				}
			}
		})
	}
}
