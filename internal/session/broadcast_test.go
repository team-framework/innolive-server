package session

import (
	"errors"
	"testing"
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
