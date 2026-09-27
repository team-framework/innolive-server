package session

import (
	"context"
	"errors"
	"testing"

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
