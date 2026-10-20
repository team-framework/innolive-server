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

// 합성 전용 워커를 정한 배포에서는 블러 세션을 합성 워커로 옮기지 않는다(#507).
// 옮기면 뒤에 오는 합성 세션이 그 워커를 나눠 쓴다.
func TestAIWorkerMovesStayWithinFaceSwapSplit(t *testing.T) {
	pool, err := ai.NewPool([]string{"a:1", "swap:1"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := pool.SetFaceSwapTargets([]string{"swap:1"}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	m := &Manager{ai: pool, sessions: map[string]*Session{
		"blur": {ID: "blur", aiTarget: "a:1"},
		"swap": {ID: "swap", aiTarget: "swap:1"},
	}}
	pool.SetTargetReady("a:1", false)
	pool.SetTargetReady("swap:1", false)
	pool.SetTargetReady("swap:1", true)
	if moves, _ := m.aiWorkerMoves(now); len(moves) != 0 {
		t.Fatalf("moves = %+v, want none: the only ready worker is the face swap worker", moves)
	}
}
