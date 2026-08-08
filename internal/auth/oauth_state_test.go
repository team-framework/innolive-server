package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOAuthStateIssueAndConsumeOnce(t *testing.T) {
	store := newOAuthStateStore(time.Minute)
	userID := uuid.New()
	state, err := store.Issue(userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 32 {
		t.Fatalf("state length = %d, want 32 hex chars", len(state))
	}

	got, ok := store.Consume(state)
	if !ok || got != userID {
		t.Fatalf("Consume = (%v, %v), want (%v, true)", got, ok, userID)
	}
	// 1회용: 같은 state 재사용은 실패해야 한다(CSRF 재시도 신호).
	if _, ok := store.Consume(state); ok {
		t.Fatal("state must be single-use")
	}
}

func TestOAuthStateUnknownAndExpired(t *testing.T) {
	store := newOAuthStateStore(time.Minute)
	if _, ok := store.Consume("never-issued"); ok {
		t.Fatal("unknown state must not consume")
	}

	now := time.Now().UTC()
	store.now = func() time.Time { return now }
	state, err := store.Issue(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, ok := store.Consume(state); ok {
		t.Fatal("expired state must not consume")
	}
}

func TestOAuthStateIssueSweepsExpiredEntries(t *testing.T) {
	store := newOAuthStateStore(time.Minute)
	now := time.Now().UTC()
	store.now = func() time.Time { return now }
	stale, err := store.Issue(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := store.Issue(uuid.New()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	_, exists := store.entries[stale]
	size := len(store.entries)
	store.mu.Unlock()
	if exists || size != 1 {
		t.Fatalf("expired entry not swept: exists=%v size=%d", exists, size)
	}
}
