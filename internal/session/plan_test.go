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
	if !live.LastMediaFrameAt().IsZero() {
		t.Fatal("no processed frame yet")
	}
}
