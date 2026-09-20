package session

import (
	"testing"
	"time"
)

func startedEgressBudget(t *testing.T, options ...StreamOptions) time.Duration {
	t.Helper()
	manager := newTestManager(t, 0)
	created, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	created.mu.Lock()
	created.rawTrackID = "video-track"
	created.mu.Unlock()

	if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/secret-key", options...); err != nil {
		t.Fatal(err)
	}
	created.mu.RLock()
	egress := created.primaryTarget().egress
	created.mu.RUnlock()
	if egress == nil {
		t.Fatal("StartStream must install an egress")
	}
	t.Cleanup(func() { manager.StopStream(created.ID) })
	return egress.ReconnectMaxElapsed()
}

// TestStartStreamKeepsDefaultBudget: 옵션을 주지 않은 호출은 종전 그대로다.
// 유튜브 경로가 여기로 들어오므로 이 값이 바뀌면 유튜브 재연결이 달라진다.
func TestStartStreamKeepsDefaultBudget(t *testing.T) {
	if budget := startedEgressBudget(t); budget != 90*time.Second {
		t.Fatalf("default budget = %v, want 90s", budget)
	}
}

// TestStartStreamAppliesReconnectBudget: 치지직처럼 종료 유예가 짧은 플랫폼은
// 서버 계층이 줄인 예산을 넘긴다. 이 배선이 끊기면 유예가 지난 뒤 재연결에
// 성공해 방송이 하나 더 생긴다.
func TestStartStreamAppliesReconnectBudget(t *testing.T) {
	budget := startedEgressBudget(t, StreamOptions{ReconnectMaxElapsed: 10 * time.Second})
	if budget != 10*time.Second {
		t.Fatalf("budget = %v, want the caller's 10s", budget)
	}
}

// TestStartStreamIgnoresZeroBudget: 옵션 구조체를 빈 값으로 넘긴 호출이
// 예산을 0으로 만들면 첫 실패에 곧바로 포기하게 된다.
func TestStartStreamIgnoresZeroBudget(t *testing.T) {
	if budget := startedEgressBudget(t, StreamOptions{}); budget != 90*time.Second {
		t.Fatalf("budget = %v, want the default 90s", budget)
	}
}
