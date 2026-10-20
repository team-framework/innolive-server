package session

import (
	"testing"
	"time"

	"inno-live-server/internal/ai"
)

// 응답 불가 워커에 배정된 세션만, 갈 곳이 있을 때만 옮긴다(#499). 방금 옮긴 세션은
// 대기가 끝난 뒤 다시 본다.
func TestAIWorkerMovesPicksSessionsOnUnreadyWorkers(t *testing.T) {
	pool, err := ai.NewPool([]string{"a:1", "b:1"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	now := time.Now()
	onDead := &Session{ID: "on-dead", aiTarget: "a:1"}
	m := &Manager{ai: pool, sessions: map[string]*Session{
		"on-dead":    onDead,
		"on-healthy": {ID: "on-healthy", aiTarget: "b:1"},
		"no-video":   {ID: "no-video"},
		"just-moved": {ID: "just-moved", aiTarget: "a:1", aiMovedAt: now.Add(-10 * time.Second)},
	}}

	if moves, retryIn := m.aiWorkerMoves(now); len(moves) != 0 || retryIn != 0 {
		t.Fatalf("moves with every worker ready = %d (retry %s), want none", len(moves), retryIn)
	}

	pool.SetTargetReady("a:1", false)
	moves, retryIn := m.aiWorkerMoves(now)
	if len(moves) != 1 || moves[0].session != onDead || moves[0].from != "a:1" {
		t.Fatalf("moves = %+v, want only on-dead from a:1", moves)
	}
	if retryIn != aiMoveCooldown-10*time.Second {
		t.Fatalf("retryIn = %s, want %s for the session moved 10s ago", retryIn, aiMoveCooldown-10*time.Second)
	}

	// 갈 곳이 없으면 옮기지 않는다. 다시 띄워도 같은 죽은 워커로 간다.
	pool.SetTargetReady("b:1", false)
	if moves, retryIn := m.aiWorkerMoves(now); len(moves) != 0 || retryIn != 0 {
		t.Fatalf("moves with no ready worker = %d (retry %s), want none", len(moves), retryIn)
	}
}
