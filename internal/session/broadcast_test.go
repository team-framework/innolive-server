package session

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBroadcastPhaseLifecycle(t *testing.T) {
	manager := newTestManager(t, 4)
	s, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, phase := s.PlatformBroadcast(); phase != BroadcastPhaseIdle {
		t.Fatalf("phase = %q, want idle", phase)
	}
	// 준비 전 라이브 전환은 거절이다.
	if _, err := manager.MarkBroadcastLive(s.ID); !errors.Is(err, ErrBroadcastNotPrepared) {
		t.Fatalf("MarkBroadcastLive() before prepare = %v, want ErrBroadcastNotPrepared", err)
	}

	broadcast := PlatformBroadcast{Provider: "youtube", BroadcastID: "bid", StreamID: "sid"}
	// 선점 없이는 기록할 수 없다.
	if _, err := manager.MarkBroadcastPrepared(s.ID, broadcast); !errors.Is(err, ErrBroadcastNotPrepared) {
		t.Fatalf("MarkBroadcastPrepared() without begin = %v, want ErrBroadcastNotPrepared", err)
	}
	if _, err := manager.BeginBroadcastPrepare(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, phase := s.PlatformBroadcast(); phase != BroadcastPhasePreparing {
		t.Fatalf("phase = %q, want preparing", phase)
	}
	if _, err := manager.MarkBroadcastPrepared(s.ID, broadcast); err != nil {
		t.Fatal(err)
	}
	stored, phase := s.PlatformBroadcast()
	if phase != BroadcastPhasePrepared || stored != broadcast {
		t.Fatalf("PlatformBroadcast() = (%+v, %q), want the prepared broadcast", stored, phase)
	}
	if s.Response().Stream.BroadcastPhase != BroadcastPhasePrepared {
		t.Fatal("stream state does not expose the prepared phase")
	}
	// 한 세션에 방송이 둘 붙으면 어느 쪽이 라이브가 되는지 알 수 없다.
	if _, err := manager.MarkBroadcastPrepared(s.ID, broadcast); !errors.Is(err, ErrBroadcastPrepared) {
		t.Fatalf("second MarkBroadcastPrepared() = %v, want ErrBroadcastPrepared", err)
	}

	if _, err := manager.MarkBroadcastLive(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, phase := s.PlatformBroadcast(); phase != BroadcastPhaseLive {
		t.Fatalf("phase = %q, want live", phase)
	}
	if _, err := manager.MarkBroadcastLive(s.ID); !errors.Is(err, ErrBroadcastLive) {
		t.Fatalf("second MarkBroadcastLive() = %v, want ErrBroadcastLive", err)
	}
}

// TestSetBroadcastSettingsRejectedAfterPrepare: 방송이 만들어진 뒤 저장값만
// 바꾸면 설정과 실물이 어긋나므로 거절한다(#142).
func TestSetBroadcastSettingsRejectedAfterPrepare(t *testing.T) {
	manager := newTestManager(t, 4)
	s, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	madeForKids := false
	if _, err := manager.SetBroadcastSettings(s.ID, YouTubeBroadcastSettings{Title: "처음", MadeForKids: &madeForKids}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginBroadcastPrepare(s.ID); err != nil {
		t.Fatal(err)
	}

	// 플랫폼 왕복이 끝나기 전(preparing)에도 막아야 한다 — 이 구간에 설정이
	// 바뀌면 만들어지는 방송과 저장값이 갈린다.
	_, err = manager.SetBroadcastSettings(s.ID, YouTubeBroadcastSettings{Title: "나중", MadeForKids: &madeForKids})
	if !errors.Is(err, ErrBroadcastPrepared) {
		t.Fatalf("SetBroadcastSettings() while preparing = %v, want ErrBroadcastPrepared", err)
	}
	if _, err := manager.MarkBroadcastPrepared(s.ID, PlatformBroadcast{Provider: "youtube", BroadcastID: "bid"}); err != nil {
		t.Fatal(err)
	}
	_, err = manager.SetBroadcastSettings(s.ID, YouTubeBroadcastSettings{Title: "나중", MadeForKids: &madeForKids})
	if !errors.Is(err, ErrBroadcastPrepared) {
		t.Fatalf("SetBroadcastSettings() after prepare = %v, want ErrBroadcastPrepared", err)
	}
	if got := s.BroadcastSettings().Title; got != "처음" {
		t.Fatalf("title = %q, want the stored settings untouched", got)
	}
}

// TestResetBroadcastPreparationReopensSettings: 준비가 실패하면 선점을 되돌려
// 설정을 다시 저장할 수 있어야 한다.
func TestResetBroadcastPreparationReopensSettings(t *testing.T) {
	manager := newTestManager(t, 4)
	s, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginBroadcastPrepare(s.ID); err != nil {
		t.Fatal(err)
	}
	manager.ResetBroadcastPreparation(s.ID)
	if _, phase := s.PlatformBroadcast(); phase != BroadcastPhaseIdle {
		t.Fatalf("phase = %q, want idle after reset", phase)
	}
	madeForKids := false
	if _, err := manager.SetBroadcastSettings(s.ID, YouTubeBroadcastSettings{Title: "다시", MadeForKids: &madeForKids}); err != nil {
		t.Fatalf("SetBroadcastSettings() after reset = %v, want success", err)
	}
	// 되돌린 뒤에는 다시 선점할 수 있다.
	if _, err := manager.BeginBroadcastPrepare(s.ID); err != nil {
		t.Fatalf("BeginBroadcastPrepare() after reset = %v, want success", err)
	}
}

// TestDeleteCleansUpPreparedBroadcast: 세션이 어떤 이유로 끝나든 라이브까지
// 가지 못한 방송은 플랫폼에서 치워야 한다. WebRTC 실패·유예 시간 초과·로그아웃·
// 명시적 삭제가 모두 Delete로 모이므로 이 경로 하나로 확인한다.
func TestDeleteCleansUpPreparedBroadcast(t *testing.T) {
	manager := newTestManager(t, 4)
	cleaned := make(chan PlatformBroadcast, 1)
	manager.SetBroadcastCleanup(func(_ uuid.UUID, broadcast PlatformBroadcast) {
		cleaned <- broadcast
	})
	s, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginBroadcastPrepare(s.ID); err != nil {
		t.Fatal(err)
	}
	broadcast := PlatformBroadcast{Provider: "youtube", BroadcastID: "bid", StreamID: "sid"}
	if _, err := manager.MarkBroadcastPrepared(s.ID, broadcast); err != nil {
		t.Fatal(err)
	}

	if err := manager.Delete(s.ID, "peer_connection_failed"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-cleaned:
		if got != broadcast {
			t.Fatalf("cleanup broadcast = %+v, want %+v", got, broadcast)
		}
	case <-time.After(time.Second):
		t.Fatal("session end did not clean up the prepared broadcast")
	}
}

// TestDeleteDoesNotCleanUpLiveBroadcast: 라이브까지 간 방송의 종료는 플랫폼
// autoStop이 맡으므로 손대지 않는다.
func TestDeleteDoesNotCleanUpLiveBroadcast(t *testing.T) {
	manager := newTestManager(t, 4)
	cleaned := make(chan PlatformBroadcast, 1)
	manager.SetBroadcastCleanup(func(_ uuid.UUID, broadcast PlatformBroadcast) {
		cleaned <- broadcast
	})
	s, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginBroadcastPrepare(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.MarkBroadcastPrepared(s.ID, PlatformBroadcast{Provider: "youtube", BroadcastID: "bid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.MarkBroadcastLive(s.ID); err != nil {
		t.Fatal(err)
	}

	if err := manager.Delete(s.ID, "user_logout"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-cleaned:
		t.Fatalf("live broadcast was deleted from the platform: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
}
