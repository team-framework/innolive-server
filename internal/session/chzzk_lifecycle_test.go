package session

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestChzzkLifecyclePreparedWithoutEgressThenLive: 치지직 세션의 상태기계
// 전 구간 — 준비(egress 없음) → 전환에서 egress 부착 → 중지 → idle → 재준비.
func TestChzzkLifecyclePreparedWithoutEgressThenLive(t *testing.T) {
	manager := newTestManager(t, 4)
	s, _, err := manager.CreateForUserWithProvider(uuid.New(), "chzzk", nil)
	if err != nil {
		t.Fatal(err)
	}
	prepared := PlatformBroadcast{Provider: "chzzk", IngestURL: "rtmp://relay/secret-key"}
	if _, err := manager.BeginBroadcastPrepare(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.MarkBroadcastPrepared(s.ID, prepared); err != nil {
		t.Fatal(err)
	}
	// egress가 없는 prepared는 중지로 놓아줄 수 있고, 치울 방송 객체는 없다.
	_, released, phase, err := manager.StopStream(s.ID)
	if err != nil || phase != BroadcastPhasePrepared || released.BroadcastID != "" {
		t.Fatalf("release = %+v/%q/%v, want prepared broadcast without a platform id", released, phase, err)
	}
	if _, phase := s.PlatformBroadcast(); phase != BroadcastPhaseIdle {
		t.Fatalf("phase = %q, want idle after release", phase)
	}
	// 놓아준 뒤 다시 중지하면 종전 계약(활성 스트림 없음)이다.
	if _, _, _, err := manager.StopStream(s.ID); !errors.Is(err, ErrStreamNotActive) {
		t.Fatalf("second stop err = %v, want ErrStreamNotActive", err)
	}

	// 재준비 → 전환에서 egress 부착.
	if _, err := manager.BeginBroadcastPrepare(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.MarkBroadcastPrepared(s.ID, prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginGoLive(s.ID); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.rawTrackID = "video-track"
	s.mu.Unlock()
	stored, _ := s.PlatformBroadcast()
	if _, err := manager.StartStream(s.ID, stored.IngestURL); err != nil {
		t.Fatal(err)
	}
	if aborted, _, err := manager.CompleteGoLive(s.ID); err != nil || aborted {
		t.Fatalf("CompleteGoLive = aborted %v, err %v", aborted, err)
	}
	if _, phase := s.PlatformBroadcast(); phase != BroadcastPhaseLive {
		t.Fatalf("phase = %q, want live", phase)
	}
	// 라이브 중지 → egress 종료 + idle.
	_, _, phase, err = manager.StopStream(s.ID)
	if err != nil || phase != BroadcastPhaseLive {
		t.Fatalf("stop live = %q/%v", phase, err)
	}
	if _, phase := s.PlatformBroadcast(); phase != BroadcastPhaseIdle {
		t.Fatalf("phase = %q, want idle after live stop", phase)
	}
	// egress가 종료 절차 중인 상태에서 곧바로 재준비할 수 있어야 한다(stop 직후
	// 재시작이 이전 Run 고루틴 종료 타이밍에 좌우되면 안 된다는 기존 계약).
	if _, err := manager.BeginBroadcastPrepare(s.ID); err != nil {
		t.Fatalf("re-prepare after live stop: %v", err)
	}
}
