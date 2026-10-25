package ai

import (
	"testing"
)

// 새로 더한 대상은 화이트리스트 이동·예열 전이므로 배정을 열기 전까지 세션을 받지
// 않는다(#515).
func TestPoolAddedTargetStaysDrainingUntilOpened(t *testing.T) {
	pool := newTestPool(t, "a:1")
	if _, err := pool.AddTarget("b:1", false); err != nil {
		t.Fatalf("AddTarget() error = %v", err)
	}

	for index := range 3 {
		if client, _ := pool.Acquire(false); client.Address() != "a:1" {
			t.Fatalf("Acquire() #%d = %q, want a:1 while b:1 drains", index, client.Address())
		}
	}

	if err := pool.SetDraining("b:1", false); err != nil {
		t.Fatalf("SetDraining(b:1, false) error = %v", err)
	}
	if client, _ := pool.Acquire(false); client.Address() != "b:1" {
		t.Fatalf("Acquire() after opening = %q, want least-loaded b:1", client.Address())
	}
	if _, err := pool.AddTarget("b:1", false); err == nil {
		t.Fatal("AddTarget() accepted a duplicate target")
	}
}

func TestPoolRemoveTargetRequiresNoActiveStreams(t *testing.T) {
	pool := newTestPool(t, "a:1", "b:1")
	if err := pool.SetDraining("a:1", true); err != nil {
		t.Fatalf("SetDraining() error = %v", err)
	}
	if client, _, ok := pool.AcquireMoveTarget(false, "b:1"); ok {
		t.Fatalf("AcquireMoveTarget() chose draining %q", client.Address())
	}
	_, release, ok := pool.AcquireMoveTarget(false, "a:1")
	if !ok {
		t.Fatal("AcquireMoveTarget() found no target, want b:1")
	}

	if err := pool.RemoveTarget("b:1"); err == nil {
		t.Fatal("RemoveTarget() removed a target with an active stream")
	}
	release()
	if err := pool.RemoveTarget("b:1"); err != nil {
		t.Fatalf("RemoveTarget() after release error = %v", err)
	}
	if err := pool.RemoveTarget("a:1"); err == nil {
		t.Fatal("RemoveTarget() removed the last target")
	}
	if got := pool.Targets(); len(got) != 1 || got[0].Address != "a:1" {
		t.Fatalf("Targets() = %+v, want only a:1", got)
	}
}

// 대상이 빠져 순서가 바뀌어도 release는 자기 대상의 활성 수만 되돌린다.
func TestPoolReleaseFollowsTargetAfterRemoval(t *testing.T) {
	pool := newTestPool(t, "a:1", "b:1", "c:1")
	_, release, ok := pool.AcquireMoveTarget(false, "a:1")
	if !ok {
		t.Fatal("AcquireMoveTarget() found no target")
	}
	if err := pool.RemoveTarget("a:1"); err != nil {
		t.Fatalf("RemoveTarget(a:1) error = %v", err)
	}
	release()
	for _, target := range pool.Targets() {
		if target.Active != 0 {
			t.Fatalf("Targets() = %+v, want no active streams after release", pool.Targets())
		}
	}
}

// 합성 전용 대상이 정해진 배포에서 새 대상은 지정한 모드의 세션만 받는다.
func TestPoolAddedTargetKeepsFaceSwapSplit(t *testing.T) {
	pool := newTestPool(t, "blur:1", "swap:1")
	if err := pool.SetFaceSwapTargets([]string{"swap:1"}); err != nil {
		t.Fatalf("SetFaceSwapTargets() error = %v", err)
	}
	if _, err := pool.AddTarget("swap:2", true); err != nil {
		t.Fatalf("AddTarget() error = %v", err)
	}
	if err := pool.SetDraining("swap:2", false); err != nil {
		t.Fatalf("SetDraining() error = %v", err)
	}

	if !pool.ServesFaceSwap("swap:2") {
		t.Fatal("ServesFaceSwap(swap:2) = false")
	}
	client, _, ok := pool.AcquireMoveTarget(true, "swap:1")
	if !ok || client.Address() != "swap:2" {
		t.Fatalf("AcquireMoveTarget(face swap) = %v, %v, want swap:2", client, ok)
	}
	if _, _, ok := pool.AcquireMoveTarget(false, "blur:1"); ok {
		t.Fatal("AcquireMoveTarget(blur) moved a blur session onto a face swap target")
	}
}
