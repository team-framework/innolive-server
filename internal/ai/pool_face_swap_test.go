package ai

import "testing"

// 합성 전용 워커를 정하면 합성 세션은 그 워커에만, 블러 세션은 나머지 워커에만
// 간다(#507). 합성 워커가 놀고 블러 워커가 붐벼도 블러를 넘기지 않는다 — 넘기면
// 뒤에 오는 합성 세션이 그 워커를 나눠 써 fps가 절반이 된다.
func TestPoolAcquireSeparatesFaceSwapTargets(t *testing.T) {
	pool := newTestPool(t, "a:1", "b:1", "swap:1")
	if err := pool.SetFaceSwapTargets([]string{"swap:1"}); err != nil {
		t.Fatal(err)
	}

	for index := range 4 {
		if client, _ := pool.Acquire(false); client.Address() == "swap:1" {
			t.Fatalf("blur Acquire() #%d chose the face swap target", index)
		}
	}
	for index := range 2 {
		if client, _ := pool.Acquire(true); client.Address() != "swap:1" {
			t.Fatalf("face swap Acquire() #%d = %q, want swap:1", index, client.Address())
		}
	}
}

// 합성 워커가 응답 불가여도 합성 세션을 블러 워커로 보내지 않는다. 블러 워커는 합성
// 모델이 없어 요청을 받으면 CPU로 지연 로드한다. 세션은 fail-closed로 막힌다.
func TestPoolFaceSwapAcquireStaysOnFaceSwapTargetsWhenUnready(t *testing.T) {
	pool := newTestPool(t, "a:1", "swap:1")
	if err := pool.SetFaceSwapTargets([]string{"swap:1"}); err != nil {
		t.Fatal(err)
	}
	pool.SetTargetReady("swap:1", false)

	if pool.HasReadyTarget(true) {
		t.Fatal("HasReadyTarget(face swap) = true with the only face swap target unready")
	}
	if !pool.HasReadyTarget(false) {
		t.Fatal("HasReadyTarget(blur) = false with a ready blur target")
	}
	if client, _ := pool.Acquire(true); client.Address() != "swap:1" {
		t.Fatalf("face swap Acquire() = %q, want swap:1 even while unready", client.Address())
	}
}

// 응답 불가 워커의 세션은 같은 모드의 워커로만 옮긴다. 블러 워커 둘이 모두 죽고
// 합성 워커만 살아 있으면 블러 세션은 옮길 곳이 없다.
func TestPoolHasReadyTargetOtherThanStaysWithinMode(t *testing.T) {
	pool := newTestPool(t, "a:1", "b:1", "swap:1")
	if err := pool.SetFaceSwapTargets([]string{"swap:1"}); err != nil {
		t.Fatal(err)
	}

	pool.SetTargetReady("a:1", false)
	if !pool.HasReadyTargetOtherThan("a:1") {
		t.Fatal("blur session on a:1 has no move target while b:1 is ready")
	}
	pool.SetTargetReady("b:1", false)
	if pool.HasReadyTargetOtherThan("a:1") {
		t.Fatal("blur session on a:1 may move to the face swap target")
	}
	if pool.HasReadyTargetOtherThan("swap:1") {
		t.Fatal("face swap session may move to a blur target")
	}
}

// 합성 전용 워커를 정하지 않은 배포는 모든 워커가 두 모드를 받는다.
func TestPoolWithoutFaceSwapTargetsServesBothModesEverywhere(t *testing.T) {
	pool := newTestPool(t, "a:1", "b:1")

	want := []string{"a:1", "b:1", "a:1", "b:1"}
	for index, expected := range want {
		if client, _ := pool.Acquire(index%2 == 1); client.Address() != expected {
			t.Fatalf("Acquire() #%d = %q, want %q", index, client.Address(), expected)
		}
	}
	if !pool.HasReadyTarget(true) || !pool.HasReadyTarget(false) {
		t.Fatal("HasReadyTarget is false for a mode while every target is ready")
	}
}

func TestPoolSetFaceSwapTargetsRejectsInvalidTargets(t *testing.T) {
	pool := newTestPool(t, "a:1", "b:1")

	if err := pool.SetFaceSwapTargets([]string{"unknown:1"}); err == nil {
		t.Fatal("SetFaceSwapTargets accepted a target outside the pool")
	}
	if err := pool.SetFaceSwapTargets([]string{"a:1", "b:1"}); err == nil {
		t.Fatal("SetFaceSwapTargets accepted every target, leaving none for blur")
	}
	if err := pool.SetFaceSwapTargets(nil); err != nil {
		t.Fatalf("SetFaceSwapTargets(nil) = %v, want nil", err)
	}
}
