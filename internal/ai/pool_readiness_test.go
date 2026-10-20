package ai

import (
	"fmt"
	"slices"
	"testing"
)

// 상태 점검이 응답 불가로 판정한 워커에는 새 세션을 보내지 않는다(#499). 런타임이
// 죽은 워커는 첫 프레임부터 실패해 세션을 블러로 잠근다.
func TestPoolAcquireSkipsUnreadyTargets(t *testing.T) {
	pool := newTestPool(t, "a:1", "b:1", "c:1")
	pool.SetTargetReady("b:1", false)

	for index := range 6 {
		if client, _ := pool.Acquire(false); client.Address() == "b:1" {
			t.Fatalf("Acquire() #%d chose unready b:1", index)
		}
	}

	// 돌아온 워커는 활성 스트림이 가장 적으므로 바로 다음 배정을 받는다.
	pool.SetTargetReady("b:1", true)
	if client, _ := pool.Acquire(false); client.Address() != "b:1" {
		t.Fatalf("Acquire() after recovery = %q, want b:1", client.Address())
	}
}

// 준비된 워커가 하나도 없어도 배정은 한다. 세션은 fail-closed로 막히고, 워커가
// 돌아오면 그대로 이어 쓴다.
func TestPoolAcquireFallsBackWhenNoTargetIsReady(t *testing.T) {
	pool := newTestPool(t, "a:1", "b:1")
	pool.SetTargetReady("a:1", false)
	pool.SetTargetReady("b:1", false)

	first, _ := pool.Acquire(false)
	second, _ := pool.Acquire(false)
	if first.Address() == second.Address() {
		t.Fatalf("Acquire() chose %q twice, want load spread across unready targets", first.Address())
	}
}

func TestPoolSetTargetReadyReportsOnlyChanges(t *testing.T) {
	pool := newTestPool(t, "a:1", "b:1")
	var events []string
	pool.SetReadinessObserver(func(target string, ready bool) {
		events = append(events, fmt.Sprintf("%s=%t", target, ready))
	})

	if pool.SetTargetReady("a:1", true) {
		t.Error("ready target set ready again reported as a change")
	}
	if !pool.SetTargetReady("a:1", false) {
		t.Error("ready -> unready not reported as a change")
	}
	if pool.SetTargetReady("a:1", false) {
		t.Error("unready target set unready again reported as a change")
	}
	if pool.SetTargetReady("unknown:1", false) {
		t.Error("unknown target reported as a change")
	}
	if pool.TargetReady("a:1") || !pool.TargetReady("b:1") {
		t.Errorf("TargetReady a:1=%t b:1=%t, want false and true", pool.TargetReady("a:1"), pool.TargetReady("b:1"))
	}
	if !pool.HasReadyTargetOtherThan("a:1") || pool.HasReadyTargetOtherThan("b:1") {
		t.Error("HasReadyTargetOtherThan does not reflect the only ready target b:1")
	}
	if !pool.SetTargetReady("a:1", true) {
		t.Error("unready -> ready not reported as a change")
	}

	if want := []string{"a:1=false", "a:1=true"}; !slices.Equal(events, want) {
		t.Fatalf("observer events = %v, want %v", events, want)
	}
}
