package auth

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
)

// ErrWithdrawalInProgress는 다른 요청이 이미 같은 계정을 지우고 있다는 뜻이다.
// 그 요청이 끝난 뒤 다시 시도하면 된다.
var ErrWithdrawalInProgress = errors.New("account withdrawal is already in progress")

// UserOperationGate는 계정 탈퇴가 도는 동안 새 사용자 범위 작업을 막는다. 탈퇴
// 전에 시작한 작업은 끝까지 두어, 그 결과가 정리 뒤에 데이터를 되살리지 못하게
// 한다.
type UserOperationGate struct {
	mu    sync.Mutex
	users map[uuid.UUID]*withdrawalGateEntry
}

type withdrawalGateEntry struct {
	withdrawing bool
	// deleted는 탈퇴가 성공한 뒤에도 켜져 있다. DB 상태 확인이 보통 삭제된 토큰을
	// 거절하지만, 탈퇴 시작 직전에 그 확인을 통과한 요청이 있을 수 있다. 이 비트를
	// 게이트에 남겨 세션·기준 얼굴 생성의 확인-입장 사이 경쟁을 막는다.
	deleted    bool
	operations int
	changed    chan struct{}
}

func NewUserOperationGate() *UserOperationGate {
	return &UserOperationGate{users: make(map[uuid.UUID]*withdrawalGateEntry)}
}

// BeginOperation은 사용자 범위 작업 하나를 들인다. 돌려준 함수는 작업이 끝날 때
// 정확히 한 번 불러야 한다. false면 탈퇴가 사용자 게이트를 쥐고 있다.
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

// BeginWithdrawal은 한 사용자의 작업을 독점한다. 이미 들인 작업이 끝나길 기다리고
// 컨텍스트 취소를 따라, 멈춘 업로드가 요청을 영원히 기다리게 하지 못한다.
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

// MarkDeleted는 DB 삭제가 커밋된 뒤 프로세스 안 입장 게이트를 영구히 닫는다. 앞서
// 활성 상태 확인을 통과한 요청도 탈퇴가 끝난 뒤 새 데이터를 만들 수 없다.
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

// IsWithdrawing은 사용자의 새 작업을 거절해야 하는지다.
func (g *UserOperationGate) IsWithdrawing(userID uuid.UUID) bool {
	if g == nil || userID == uuid.Nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.users[userID]
	return entry != nil && (entry.withdrawing || entry.deleted)
}
