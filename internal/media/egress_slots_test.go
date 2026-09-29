package media

import (
	"context"
	"errors"
	"fmt"
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

// TestBudgetForwardsSignalFromTimedOutWaiter: 반납 신호가 막 시간이 다한
// 대기자에게 전달되면 그 신호는 버려진다. 다음 대기자에게 넘기지 않으면
// 자리가 비어 있는데도 남은 요청이 자기 시간이 다할 때까지 기다리다 실패한다.
func TestBudgetForwardsSignalFromTimedOutWaiter(t *testing.T) {
	budget := NewEgressSlotBudget(1, 0)
	held, err := budget.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// 대기자 둘을 세운다: 하나는 곧 시간이 다하고, 하나는 계속 기다린다.
	shortCtx, cancelShort := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancelShort()
	shortDone := make(chan struct{})
	go func() {
		defer close(shortDone)
		if lease, err := budget.Acquire(shortCtx); err == nil {
			lease.Release()
		}
	}()
	time.Sleep(20 * time.Millisecond)

	longDone := make(chan error, 1)
	go func() {
		lease, err := budget.Acquire(context.Background())
		if lease != nil {
			lease.Release()
		}
		longDone <- err
	}()

	// 첫 대기자의 시간이 다하는 순간에 맞춰 반납한다.
	time.Sleep(60 * time.Millisecond)
	held.Release()
	<-shortDone

	select {
	case err := <-longDone:
		if err != nil {
			t.Fatalf("the remaining waiter must get the free slot: %v", err)
		}
	case <-time.After(egressSlotWait):
		t.Fatal("the free slot was never handed to the remaining waiter")
	}
}

func TestEgressUnitsMatchBM(t *testing.T) {
	cases := []struct {
		highRes bool
		count   int
		want    int
	}{
		{false, 1, 1}, {true, 1, 2}, {false, 2, 2}, {true, 2, 3}, {true, 0, 0},
	}
	for _, test := range cases {
		if got := egressUnits(test.highRes, test.count); got != test.want {
			t.Fatalf("egressUnits(%v, %d) = %d, want %d", test.highRes, test.count, got, test.want)
		}
	}
}

func TestBudgetChargesUnitsPerOwner(t *testing.T) {
	budget := NewEgressSlotBudget(13, 0)
	ctx := context.Background()
	fhd := EgressClaim{Owner: "fhd-session", HighRes: true, Group: "plasma"}

	first, err := budget.AcquireClaim(ctx, fhd)
	if err != nil || budget.Used() != 2 {
		t.Fatalf("FHD single: used=%d err=%v, want 2", budget.Used(), err)
	}
	second, err := budget.AcquireClaim(ctx, fhd)
	if err != nil || budget.Used() != 3 {
		t.Fatalf("FHD simulcast: used=%d err=%v, want 3", budget.Used(), err)
	}
	// 한쪽이 먼저 끝나면 FHD 단독(2)으로 돌아가야 한다 — 1이 되면 과소 계산이다.
	first.Release()
	if budget.Used() != 2 {
		t.Fatalf("after first target ended: used=%d, want 2", budget.Used())
	}
	second.Release()
	if budget.Used() != 0 || len(budget.UsedByGroup()) != 0 {
		t.Fatalf("after all ended: used=%d groups=%v", budget.Used(), budget.UsedByGroup())
	}

	hd := EgressClaim{Owner: "hd-session", Group: "beam"}
	a, _ := budget.AcquireClaim(ctx, hd)
	b, _ := budget.AcquireClaim(ctx, hd)
	if budget.Used() != 2 || budget.UsedByGroup()["beam"] != 2 {
		t.Fatalf("720p simulcast: used=%d groups=%v, want 2", budget.Used(), budget.UsedByGroup())
	}
	a.Release()
	b.Release()
}

func TestBudgetTierReserves(t *testing.T) {
	// Spark 2 · Beam 7 · Plasma 4 = 13.
	budget := NewEgressSlotBudget(13, 0)
	budget.SetTierReserves([]int{2, 7, 4})
	ctx := context.Background()
	claim := func(owner string, tier int) EgressClaim { return EgressClaim{Owner: owner, Tier: tier} }

	// Spark는 자기 몫 2까지만 — 상위 몫이 비어 있어도 쓰지 못한다.
	spark1, err := budget.AcquireClaim(ctx, claim("s1", 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := budget.AcquireClaim(ctx, claim("s2", 0)); err != nil {
		t.Fatal(err)
	}
	_, err = budget.AcquireClaim(ctx, claim("s3", 0))
	if !errors.Is(err, ErrEgressTierUnitsExhausted) || !errors.Is(err, ErrEgressSlotsExhausted) {
		t.Fatalf("third spark: err=%v, want tier exhaustion", err)
	}

	// Beam은 자기 몫 7을 채운 뒤 Plasma 몫(4)은 쓰지 못한다.
	var beams []*EgressLease
	for i := 0; i < 7; i++ {
		lease, err := budget.AcquireClaim(ctx, claim(fmt.Sprintf("b%d", i), 1))
		if err != nil {
			t.Fatalf("beam %d: %v", i, err)
		}
		beams = append(beams, lease)
	}
	if _, err := budget.AcquireClaim(ctx, claim("b7", 1)); !errors.Is(err, ErrEgressTierUnitsExhausted) {
		t.Fatalf("beam into plasma reserve: err=%v", err)
	}
	// Plasma는 자기 몫 4를 그대로 쓴다.
	if _, err := budget.AcquireClaim(ctx, EgressClaim{Owner: "p1", Tier: 2, HighRes: true}); err != nil {
		t.Fatalf("plasma FHD: %v", err)
	}
	// Spark 자리가 비면 Beam이 빌려 쓸 수 있다(하위 몫).
	spark1.Release()
	if _, err := budget.AcquireClaim(ctx, claim("b7", 1)); err != nil {
		t.Fatalf("beam borrowing free spark unit: %v", err)
	}
	for _, lease := range beams {
		lease.Release()
	}
}

func TestBudgetPlasmaBorrowsLowerTiers(t *testing.T) {
	budget := NewEgressSlotBudget(13, 0)
	budget.SetTierReserves([]int{2, 7, 4})
	ctx := context.Background()
	// 아무도 없으면 Plasma는 총량 전부를 쓸 수 있다.
	for i := 0; i < 6; i++ {
		if _, err := budget.AcquireClaim(ctx, EgressClaim{Owner: fmt.Sprintf("p%d", i), Tier: 2, HighRes: true}); err != nil {
			t.Fatalf("plasma FHD %d: %v", i, err)
		}
	}
	if budget.Used() != 12 {
		t.Fatalf("plasma used %d units, want 12 (borrowing beyond its own 4)", budget.Used())
	}
	// 플랜이 없는 세션(벤치)은 등급 규칙을 받지 않는다.
	if _, err := budget.AcquireClaim(ctx, EgressClaim{Owner: "bench", Tier: EgressTierUnrestricted}); err != nil {
		t.Fatalf("unrestricted: %v", err)
	}
}

func TestBudgetRejectsUnitsOverCapacity(t *testing.T) {
	budget := NewEgressSlotBudget(3, 0)
	ctx := context.Background()
	hold, err := budget.AcquireClaim(ctx, EgressClaim{Owner: "a", HighRes: true})
	if err != nil {
		t.Fatal(err)
	}
	// 남은 1유닛에 FHD(2)는 들어가지 않는다.
	if _, err := budget.AcquireClaim(ctx, EgressClaim{Owner: "b", HighRes: true}); !errors.Is(err, ErrEgressSlotsExhausted) {
		t.Fatalf("FHD over capacity: err=%v", err)
	}
	if budget.Used() != 2 {
		t.Fatalf("rejected claim changed usage: %d", budget.Used())
	}
	hold.Release()
	if _, err := budget.AcquireClaim(ctx, EgressClaim{Owner: "b", HighRes: true}); err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
}

func TestBudgetWakesLargeWaiterAfterSmallRelease(t *testing.T) {
	budget := NewEgressSlotBudget(2, 0)
	ctx := context.Background()
	a, _ := budget.AcquireClaim(ctx, EgressClaim{Owner: "a"})
	b, _ := budget.AcquireClaim(ctx, EgressClaim{Owner: "b"})
	done := make(chan error, 1)
	go func() {
		_, err := budget.AcquireClaim(ctx, EgressClaim{Owner: "fhd", HighRes: true})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	a.Release()
	b.Release()
	if err := <-done; err != nil {
		t.Fatalf("FHD waiter not admitted after two releases: %v", err)
	}
}

// 해상도 변경(#283): 늘어나는 유닛만 판정하고, 자리가 없으면 기존 해상도를 유지한다.
func TestBudgetChangeResolution(t *testing.T) {
	budget := NewEgressSlotBudget(4, 0)
	ctx := context.Background()
	// 720p 동시 송출 2유닛 + 다른 세션 1유닛.
	for i := 0; i < 2; i++ {
		if _, err := budget.AcquireClaim(ctx, EgressClaim{Owner: "a", Group: "beam"}); err != nil {
			t.Fatal(err)
		}
	}
	other, err := budget.AcquireClaim(ctx, EgressClaim{Owner: "b", Group: "beam"})
	if err != nil {
		t.Fatal(err)
	}
	// FHD 동시는 3유닛 — 1이 늘어 4, 총량 안이다.
	if err := budget.ChangeResolution("a", true); err != nil || budget.Used() != 4 || budget.UsedByGroup()["beam"] != 4 {
		t.Fatalf("upgrade: err=%v used=%d groups=%v, want 4", err, budget.Used(), budget.UsedByGroup())
	}
	// 다른 세션이 FHD로 가려면 1이 더 필요하다 — 만석이라 거절하고 그대로 둔다.
	if err := budget.ChangeResolution("b", true); !errors.Is(err, ErrEgressSlotsExhausted) || budget.Used() != 4 {
		t.Fatalf("full upgrade: err=%v used=%d, want exhausted and unchanged", err, budget.Used())
	}
	// 거절된 뒤 반납하면 720p 기준(1)만 돌아와야 한다.
	other.Release()
	if budget.Used() != 3 {
		t.Fatalf("after release used=%d, want 3", budget.Used())
	}
	// 낮추면 바로 반납한다. 이후 송출 추가는 바뀐 해상도로 센다.
	if err := budget.ChangeResolution("a", false); err != nil || budget.Used() != 2 {
		t.Fatalf("downgrade: err=%v used=%d, want 2", err, budget.Used())
	}
	if _, err := budget.AcquireClaim(ctx, EgressClaim{Owner: "a", HighRes: true, Group: "beam"}); err != nil || budget.Used() != 3 {
		t.Fatalf("third target after downgrade: err=%v used=%d, want 3 (720p ×3)", err, budget.Used())
	}
	// 송출 중이 아닌 소유자는 바꿀 것이 없다.
	if err := budget.ChangeResolution("idle", true); err != nil || budget.Used() != 3 {
		t.Fatalf("idle owner: err=%v used=%d", err, budget.Used())
	}
}

func TestBudgetChangeResolutionHonorsTierReserves(t *testing.T) {
	// Spark 2 · Beam 1 · Plasma 1 = 4. Spark 720p 1유닛이 FHD(2)로 가는 건 자기 몫 안이다.
	budget := NewEgressSlotBudget(4, 0)
	budget.SetTierReserves([]int{2, 1, 1})
	if _, err := budget.AcquireClaim(context.Background(), EgressClaim{Owner: "s", Tier: 0}); err != nil {
		t.Fatal(err)
	}
	if err := budget.ChangeResolution("s", true); err != nil {
		t.Fatalf("within own reserve: %v", err)
	}
	// Beam 720p 동시 2유닛(자기 1 + Spark 빈 몫 1) → FHD 동시 3은 Plasma 빈 몫을 넘본다.
	budget2 := NewEgressSlotBudget(4, 0)
	budget2.SetTierReserves([]int{1, 1, 2})
	for i := 0; i < 2; i++ {
		if _, err := budget2.AcquireClaim(context.Background(), EgressClaim{Owner: "b", Tier: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := budget2.ChangeResolution("b", true); !errors.Is(err, ErrEgressTierUnitsExhausted) || budget2.Used() != 2 {
		t.Fatalf("beam into plasma reserve: err=%v used=%d, want tier exhaustion and unchanged", err, budget2.Used())
	}
}

// 방식 전환(#300) 사전 확인: 지금 쥔 유닛은 반납될 것으로 보고, 잡지는 않는다.
func TestBudgetCheckClaim(t *testing.T) {
	budget := NewEgressSlotBudget(4, 0)
	budget.SetTierReserves([]int{1, 1, 2})
	ctx := context.Background()
	// Beam FHD 단독 2유닛 + 다른 Plasma 1유닛.
	if _, err := budget.AcquireClaim(ctx, EgressClaim{Owner: "beam", HighRes: true, Tier: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := budget.AcquireClaim(ctx, EgressClaim{Owner: "plasma", Tier: 2}); err != nil {
		t.Fatal(err)
	}
	// Beam FHD 단독(2) → 720p 동시(2): 반납분 2를 빼면 3/4 — 된다. 잡지 않으므로 그대로 3.
	if err := budget.CheckClaim(EgressClaim{Owner: "beam", Tier: 1}, 2); err != nil || budget.Used() != 3 {
		t.Fatalf("720p simulcast: err=%v used=%d", err, budget.Used())
	}
	// Beam FHD 동시(3): 4/4지만 Plasma 빈 몫 1을 넘본다.
	if err := budget.CheckClaim(EgressClaim{Owner: "beam", HighRes: true, Tier: 1}, 2); !errors.Is(err, ErrEgressTierUnitsExhausted) {
		t.Fatalf("fhd simulcast: err=%v, want tier exhaustion", err)
	}
	// Plasma 720p 단독(1) → FHD 동시(3): 2+3=5 > 4.
	if err := budget.CheckClaim(EgressClaim{Owner: "plasma", HighRes: true, Tier: 2}, 2); !errors.Is(err, ErrEgressSlotsExhausted) {
		t.Fatalf("over capacity: err=%v", err)
	}
	// 송출 중이 아닌 소유자는 전부 새로 센다.
	if err := budget.CheckClaim(EgressClaim{Owner: "new", Tier: 2}, 1); err != nil {
		t.Fatalf("new owner: %v", err)
	}
}

// 보류한 유닛은 다른 소유자가 가져가지 못하고, 보류한 소유자의 화질 올리기에
// 먼저 쓰인다(#278).
func TestBudgetHoldReservesUnitsForOwner(t *testing.T) {
	budget := NewEgressSlotBudget(2, 0)
	ctx := context.Background()
	if _, err := budget.AcquireClaim(ctx, EgressClaim{Owner: "a", Group: "plasma"}); err != nil {
		t.Fatal(err)
	}
	if err := budget.Hold(EgressClaim{Owner: "a", Group: "plasma"}, 1); err != nil || budget.Used() != 2 || budget.Held("a") != 1 {
		t.Fatalf("hold: err=%v used=%d held=%d, want 2/1", err, budget.Used(), budget.Held("a"))
	}
	// 남은 자리가 없다 — 다른 소유자는 못 잡는다.
	if _, _, err := budget.tryAcquire(EgressClaim{Owner: "b", Group: "beam"}); !errors.Is(err, ErrEgressSlotsExhausted) {
		t.Fatalf("other acquire err=%v, want exhausted while held", err)
	}
	// 보류한 소유자의 확인은 자기 보류분을 제 몫으로 센다.
	if err := budget.CheckClaim(EgressClaim{Owner: "a", HighRes: true}, 1); err != nil {
		t.Fatalf("CheckClaim(fhd) = %v, want room from own hold", err)
	}
	// 화질 올리기는 보류분을 소비한다 — 점유는 그대로 2.
	if err := budget.ChangeResolution("a", true); err != nil || budget.Used() != 2 || budget.Held("a") != 0 {
		t.Fatalf("upgrade: err=%v used=%d held=%d, want 2/0", err, budget.Used(), budget.Held("a"))
	}
}

// 전환은 옛 송출을 모두 내린 뒤 새로 잡는다. 그 사이에도 보류분은 남아 새 송출이
// 먼저 쓴다.
func TestBudgetHoldSurvivesReopen(t *testing.T) {
	budget := NewEgressSlotBudget(2, 0)
	ctx := context.Background()
	lease, err := budget.AcquireClaim(ctx, EgressClaim{Owner: "a", Group: "beam"})
	if err != nil {
		t.Fatal(err)
	}
	if err := budget.Hold(EgressClaim{Owner: "a", Group: "beam"}, 1); err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if budget.Used() != 1 || budget.Held("a") != 1 {
		t.Fatalf("after stop used=%d held=%d, want only the hold", budget.Used(), budget.Held("a"))
	}
	if _, err := budget.AcquireClaim(ctx, EgressClaim{Owner: "a", HighRes: true, Group: "beam"}); err != nil {
		t.Fatalf("reopen as FHD: %v", err)
	}
	if budget.Used() != 2 || budget.Held("a") != 0 || budget.UsedByGroup()["beam"] != 2 {
		t.Fatalf("reopened used=%d held=%d groups=%v, want 2/0", budget.Used(), budget.Held("a"), budget.UsedByGroup())
	}
}

func TestBudgetReleaseHoldFreesUnitsAndRejectsWhenFull(t *testing.T) {
	budget := NewEgressSlotBudget(1, 0)
	if err := budget.Hold(EgressClaim{Owner: "a", Group: "beam"}, 1); err != nil {
		t.Fatal(err)
	}
	if err := budget.Hold(EgressClaim{Owner: "b", Group: "beam"}, 1); !errors.Is(err, ErrEgressSlotsExhausted) {
		t.Fatalf("second hold err=%v, want exhausted", err)
	}
	budget.ReleaseHold("a")
	budget.ReleaseHold("a") // 두 번 불러도 안전하다
	if budget.Used() != 0 || len(budget.UsedByGroup()) != 0 {
		t.Fatalf("after release used=%d groups=%v, want empty", budget.Used(), budget.UsedByGroup())
	}
	if _, _, err := budget.tryAcquire(EgressClaim{Owner: "b", Group: "beam"}); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}
