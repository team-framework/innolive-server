package auth

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
)

// ErrWithdrawalInProgress means that another request is already deleting the
// same account. The caller may retry after that request finishes.
var ErrWithdrawalInProgress = errors.New("account withdrawal is already in progress")

// UserOperationGate blocks new user-scoped work while an account withdrawal is
// running. Operations that started before the withdrawal are allowed to finish
// so their result cannot recreate data after cleanup.
type UserOperationGate struct {
	mu    sync.Mutex
	users map[uuid.UUID]*withdrawalGateEntry
}

type withdrawalGateEntry struct {
	withdrawing bool
	// deleted remains set after a successful withdrawal. The database status
	// check normally rejects a deleted token, but a request can pass that check
	// just before withdrawal starts. Keeping this bit in the gate closes that
	// check-to-admission race for session and reference creation.
	deleted    bool
	operations int
	changed    chan struct{}
}

func NewUserOperationGate() *UserOperationGate {
	return &UserOperationGate{users: make(map[uuid.UUID]*withdrawalGateEntry)}
}

// BeginOperation admits one user-scoped operation. The returned function must
// be called exactly once when the operation finishes. A false result means a
// withdrawal owns the user gate.
func (g *UserOperationGate) BeginOperation(userID uuid.UUID) (release func(), admitted bool) {
	if g == nil || userID == uuid.Nil {
		return func() {}, true
	}

	g.mu.Lock()
	entry := g.users[userID]
	if entry == nil {
		entry = &withdrawalGateEntry{changed: make(chan struct{})}
		g.users[userID] = entry
	}
	if entry.withdrawing || entry.deleted {
		g.mu.Unlock()
		return nil, false
	}
	entry.operations++
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() { g.endOperation(userID, entry) })
	}, true
}

func (g *UserOperationGate) endOperation(userID uuid.UUID, entry *withdrawalGateEntry) {
	g.mu.Lock()
	if entry.operations > 0 {
		entry.operations--
	}
	if entry.operations == 0 {
		close(entry.changed)
		entry.changed = make(chan struct{})
		if !entry.withdrawing && !entry.deleted && g.users[userID] == entry {
			delete(g.users, userID)
		}
	}
	g.mu.Unlock()
}

// BeginWithdrawal acquires exclusive ownership of one user's work. It waits
// for operations that were already admitted and honors context cancellation so
// a stalled upload cannot make a request wait forever.
func (g *UserOperationGate) BeginWithdrawal(ctx context.Context, userID uuid.UUID) (release func(), err error) {
	if g == nil || userID == uuid.Nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	g.mu.Lock()
	entry := g.users[userID]
	if entry == nil {
		entry = &withdrawalGateEntry{changed: make(chan struct{})}
		g.users[userID] = entry
	}
	if entry.withdrawing {
		g.mu.Unlock()
		return nil, ErrWithdrawalInProgress
	}
	if entry.deleted {
		g.mu.Unlock()
		return nil, ErrUserInactive
	}
	entry.withdrawing = true
	for entry.operations > 0 {
		changed := entry.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			g.mu.Lock()
			entry.withdrawing = false
			if entry.operations == 0 && !entry.deleted && g.users[userID] == entry {
				delete(g.users, userID)
			}
			g.mu.Unlock()
			return nil, ctx.Err()
		}
		g.mu.Lock()
	}
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			entry.withdrawing = false
			if entry.operations == 0 && !entry.deleted && g.users[userID] == entry {
				delete(g.users, userID)
			}
			g.mu.Unlock()
		})
	}, nil
}

// MarkDeleted permanently closes the in-process admission gate after the
// database deletion has committed. A request that passed an earlier active
// status check can therefore not create new data after withdrawal returns.
func (g *UserOperationGate) MarkDeleted(userID uuid.UUID) {
	if g == nil || userID == uuid.Nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.users[userID]
	if entry == nil {
		entry = &withdrawalGateEntry{changed: make(chan struct{})}
		g.users[userID] = entry
	}
	entry.deleted = true
}

// IsWithdrawing reports whether new work for the user must be rejected.
func (g *UserOperationGate) IsWithdrawing(userID uuid.UUID) bool {
	if g == nil || userID == uuid.Nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.users[userID]
	return entry != nil && (entry.withdrawing || entry.deleted)
}
