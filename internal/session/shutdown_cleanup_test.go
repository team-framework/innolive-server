package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// 세션을 닫을 때 남은 플랫폼 방송 정리(라이브 종료 호출)는 고루틴으로 돈다. 종료가
// 이를 기다리지 않으면 프로세스가 먼저 끝나 시청자에게 방송이 1분 넘게 남는다(P1-26).
func TestWaitForDeletesWaitsForPlatformBroadcastCleanup(t *testing.T) {
	manager := newTestManager(t, 0)
	release := make(chan struct{})
	finished := make(chan struct{})
	manager.SetBroadcastCleanup(func(uuid.UUID, PlatformBroadcast, BroadcastPhase) {
		<-release
		close(finished)
	})
	live, _, err := manager.CreateForUser(uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginBroadcastPrepare(live.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.MarkBroadcastPrepared(live.ID, PlatformBroadcast{Provider: "youtube", BroadcastID: "broadcast-1"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Delete(live.ID, "shutdown"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := manager.WaitForDeletes(ctx); !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		t.Fatalf("WaitForDeletes = %v while the platform cleanup was still running", err)
	}
	close(release)
	if err := manager.WaitForDeletes(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("WaitForDeletes returned before the platform cleanup finished")
	}
}
