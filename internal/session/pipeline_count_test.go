package session

import "testing"

// 카메라를 전환하면 새 파이프라인이 뜬 뒤 이전 파이프라인이 끝난다. 이전 것의
// 종료가 새 등록까지 지우면 종료 중인 세션의 파이프라인이 MaxSessions 사용량과
// 종료 대기에서 빠진다(P1-4).
func TestPipelineDoneKeepsReplacementRegistered(t *testing.T) {
	manager := newTestManager(t, 0)
	manager.pipelineStarted("session")
	manager.pipelineStarted("session")
	manager.pipelineDone("session")

	manager.mu.RLock()
	running := manager.pipelines["session"]
	manager.mu.RUnlock()
	if running != 1 {
		t.Fatalf("running pipelines after the old one ended = %d, want the replacement still counted", running)
	}

	manager.pipelineDone("session")
	manager.mu.RLock()
	_, left := manager.pipelines["session"]
	manager.mu.RUnlock()
	if left {
		t.Fatal("session still registered after its last pipeline ended")
	}
}
