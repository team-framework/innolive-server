package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"inno-live-server/internal/plan"

	"github.com/google/uuid"
)

func TestCreateCarriesOwnerPlan(t *testing.T) {
	manager := newTestManager(t, 0)
	userID := uuid.New()
	manager.SetPlanResolver(func(_ context.Context, id uuid.UUID) (plan.Plan, error) {
		if id != userID {
			t.Fatalf("resolver asked for %s, want %s", id, userID)
		}
		return plan.Beam, nil
	})

	liveSession, _, err := manager.CreateForUser(userID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if liveSession.Plan != plan.Beam {
		t.Fatalf("session plan = %q, want beam", liveSession.Plan)
	}

	guest, _, err := manager.CreateForGuest("guest-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if guest.Plan != "" {
		t.Fatalf("guest session plan = %q, want empty", guest.Plan)
	}
}

func TestCreateFailsWhenPlanLookupFails(t *testing.T) {
	manager := newTestManager(t, 1)
	lookupErr := errors.New("database unavailable")
	manager.SetPlanResolver(func(context.Context, uuid.UUID) (plan.Plan, error) {
		return "", lookupErr
	})

	if _, _, err := manager.CreateForUser(uuid.New(), nil); !errors.Is(err, lookupErr) {
		t.Fatalf("create error = %v, want plan lookup error", err)
	}
	// 실패한 생성이 자리를 쥐고 남으면 안 된다.
	if active, _ := manager.Capacity(); active != 0 {
		t.Fatalf("active sessions after failed create = %d, want 0", active)
	}
}

func TestNoticesAreDeduplicatedAndExposed(t *testing.T) {
	manager := newTestManager(t, 0)
	live, _, err := manager.CreateForUser(uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if !live.AddNotice("monthly_usage_80", at) || live.AddNotice("monthly_usage_80", at.Add(time.Minute)) {
		t.Fatal("a notice code must be added exactly once")
	}
	live.AddNotice("broadcast_limit_30m", at)
	notices := live.Response().Notices
	if len(notices) != 2 || notices[0].Code != "monthly_usage_80" || notices[1].Code != "broadcast_limit_30m" {
		t.Fatalf("notices = %+v", notices)
	}
	// 송출이 없으면 라이브 대상도 없고, 프레임 기록도 없다.
	if targets, unpaused := live.BroadcastActivity(); len(targets) != 0 || unpaused != 0 {
		t.Fatalf("activity = %v/%d, want none", targets, unpaused)
	}
	if !live.MediaIdleSince().IsZero() {
		t.Fatal("no processed frame yet")
	}
}

// 입력 없음 시계는 재개·송출 시작에서 다시 시작해야 한다. 모두 멈춘 동안에는
// AI 입력이 멈춰 처리 프레임이 없으므로, 오래 멈췄다 재개한 직후 마지막 프레임
// 시각만 보면 곧바로 입력 없음으로 판정된다.
func TestMediaIdleClockRestartsOnResume(t *testing.T) {
	manager := newTestManager(t, 0)
	live, _, err := manager.CreateForUser(uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !live.MediaIdleSince().IsZero() {
		t.Fatal("no frame and no broadcast yet: idle clock must not run")
	}
	stale := time.Now().Add(-20 * time.Minute)
	live.lastMediaFrameNano.Store(stale.UnixNano())
	resumed := time.Now()
	live.restartMediaIdleClock(resumed)
	if got := live.MediaIdleSince(); got.Before(resumed) {
		t.Fatalf("idle since %v, want at or after resume %v", got, resumed)
	}
	// 재개 뒤 프레임이 오면 그 시각이 기준이다.
	later := resumed.Add(time.Minute)
	live.lastMediaFrameNano.Store(later.UnixNano())
	if got := live.MediaIdleSince(); !got.Equal(time.Unix(0, later.UnixNano())) {
		t.Fatalf("idle since %v, want latest frame %v", got, later)
	}
}

func TestBroadcastRemainingIsExposedAndCleared(t *testing.T) {
	manager := newTestManager(t, 0)
	live, _, err := manager.CreateForUser(uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if live.Response().BroadcastRemainingSeconds != nil {
		t.Fatal("not broadcasting: remaining must be null")
	}
	remaining := 90 * time.Minute
	live.SetBroadcastRemaining(&remaining)
	if got := live.Response().BroadcastRemainingSeconds; got == nil || *got != 5400 {
		t.Fatalf("remaining = %v, want 5400", got)
	}
	live.SetBroadcastRemaining(nil)
	if live.Response().BroadcastRemainingSeconds != nil {
		t.Fatal("cleared remaining must be null")
	}
}
