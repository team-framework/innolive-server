package media

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestNilBudgetIsUnlimited: 설정하지 않은 배포는 종전 그대로 제한도 카드
// 배정도 없어야 한다.
func TestNilBudgetIsUnlimited(t *testing.T) {
	if budget := NewEgressSlotBudget(0, 0); budget != nil {
		t.Fatal("an unconfigured budget must be nil (unlimited)")
	}
	var budget *EgressSlotBudget
	lease, err := budget.Acquire(context.Background())
	if err != nil {
		t.Fatalf("nil budget must not fail: %v", err)
	}
	if lease.Device() != -1 {
		t.Fatalf("Device() = %d, want -1 (unassigned)", lease.Device())
	}
	lease.Release() // nil 리스에도 안전해야 한다
}

// TestBudgetRejectsWhenFull: 상한을 넘으면 기다려 본 뒤 거절한다. 무한정
// 기다리면 사용자에게는 멈춘 버튼으로 보인다.
func TestBudgetRejectsWhenFull(t *testing.T) {
	budget := NewEgressSlotBudget(2, 0)
	first, err := budget.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := budget.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if budget.Used() != 2 {
		t.Fatalf("Used() = %d, want 2", budget.Used())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := budget.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a full budget must not hand out a third slot: %v", err)
	}

	// 반납하면 다시 잡힌다.
	first.Release()
	third, err := budget.Acquire(context.Background())
	if err != nil {
		t.Fatalf("a released slot must be reusable: %v", err)
	}
	third.Release()
}

// TestBudgetExhaustedAfterWait: 대기 시간이 지나면 ErrEgressSlotsExhausted다.
func TestBudgetExhaustedAfterWait(t *testing.T) {
	budget := NewEgressSlotBudget(1, 0)
	held, err := budget.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	start := time.Now()
	_, err = budget.Acquire(context.Background())
	if !errors.Is(err, ErrEgressSlotsExhausted) {
		t.Fatalf("error = %v, want ErrEgressSlotsExhausted", err)
	}
	if elapsed := time.Since(start); elapsed < egressSlotWait {
		t.Fatalf("waited %v, want at least %v before giving up", elapsed, egressSlotWait)
	}
}

// TestBudgetWakesWaiterOnRelease: 종료 직후의 반납(실측 0.6초)은 대기 중인
// 요청이 흡수해야 한다 — 그게 짧게 기다리는 이유다.
func TestBudgetWakesWaiterOnRelease(t *testing.T) {
	budget := NewEgressSlotBudget(1, 0)
	held, err := budget.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		lease, err := budget.Acquire(context.Background())
		if lease != nil {
			lease.Release()
		}
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	held.Release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the waiter must get the released slot: %v", err)
		}
	case <-time.After(egressSlotWait):
		t.Fatal("the waiter was not woken by the release")
	}
}

// TestBudgetPicksLeastLoadedCard: 시작 순서만 세는 라운드로빈은 종료가 한쪽에
// 몰리면 편중되어, 총량이 남아도 한 카드가 먼저 한계에 닿는다.
func TestBudgetPicksLeastLoadedCard(t *testing.T) {
	budget := NewEgressSlotBudget(0, 2)
	first, _ := budget.Acquire(context.Background())
	second, _ := budget.Acquire(context.Background())
	if first.Device() == second.Device() {
		t.Fatalf("two slots landed on the same card (%d)", first.Device())
	}

	// 첫 카드만 비운 뒤에는 다음 두 자리가 모두 그 카드로 가야 한다.
	freed := first.Device()
	first.Release()
	third, _ := budget.Acquire(context.Background())
	if third.Device() != freed {
		t.Fatalf("third slot went to card %d, want the freed card %d", third.Device(), freed)
	}
	_ = second
}

// TestBudgetHonorsPerCardLimit: 총량이 남아도 카드가 한계면 시작할 수 없다.
// NVENC는 13번째에서 incompatible client key (21)로 실패한다.
func TestBudgetHonorsPerCardLimit(t *testing.T) {
	budget := NewEgressSlotBudget(0, 1)
	leases := make([]*EgressLease, 0, nvencSessionsPerCard)
	for index := 0; index < nvencSessionsPerCard; index++ {
		lease, err := budget.Acquire(context.Background())
		if err != nil {
			t.Fatalf("slot %d: %v", index, err)
		}
		leases = append(leases, lease)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := budget.Acquire(ctx); err == nil {
		t.Fatalf("card limit %d must be enforced", nvencSessionsPerCard)
	}
	for _, lease := range leases {
		lease.Release()
	}
}

// TestLeaseReleaseIsIdempotent: 정리 경로가 여러 갈래라 중복 반납이 실제로
// 일어난다. 두 번 세면 회계가 음수로 새고 상한이 무의미해진다.
func TestLeaseReleaseIsIdempotent(t *testing.T) {
	budget := NewEgressSlotBudget(1, 1)
	lease, err := budget.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	lease.Release()
	if used := budget.Used(); used != 0 {
		t.Fatalf("Used() = %d, want 0", used)
	}
	if cards := budget.PerCard(); cards[0] != 0 {
		t.Fatalf("PerCard() = %v, want the card released once", cards)
	}
}

// TestBudgetConcurrentAcquire: 동시 요청이 상한을 넘겨 잡으면 NVENC가
// 실패하므로 경합에서도 상한이 지켜져야 한다.
func TestBudgetConcurrentAcquire(t *testing.T) {
	const capacity = 4
	budget := NewEgressSlotBudget(capacity, 2)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := make([]*EgressLease, 0, capacity)
	for index := 0; index < capacity*3; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			lease, err := budget.Acquire(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			granted = append(granted, lease)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(granted) != capacity {
		t.Fatalf("granted %d slots, want exactly %d", len(granted), capacity)
	}
	if used := budget.Used(); used != capacity {
		t.Fatalf("Used() = %d, want %d", used, capacity)
	}
	for _, lease := range granted {
		lease.Release()
	}
	if used := budget.Used(); used != 0 {
		t.Fatalf("Used() after release = %d, want 0", used)
	}
}
