package media

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrEgressSlotsExhausted는 송출 자리가 없어 egress를 시작할 수 없는 경우다.
// 자리는 다른 방송이 끝나야 나므로 즉시 재시도해도 풀리지 않는다 — 호출자는
// 대기가 아니라 사용자에게 알릴 실패로 다뤄야 한다.
var ErrEgressSlotsExhausted = errors.New("no egress slot is available")

const (
	// nvencSessionsPerCard는 카드 한 장이 동시에 쥘 수 있는 NVENC 세션 수다
	// (RTX 5070 Ti 실측, 13번째에서 incompatible client key (21)).
	nvencSessionsPerCard = 12
	// egressSlotWait는 만석일 때 기다려 보는 시간이다. 종료 직후의 반납은
	// 0.6초 안에 끝나는 것이 실측됐으므로(프로덕션 kill 테스트) 그런 전이
	// 구간은 흡수하고, 진짜 만석은 바로 알린다.
	egressSlotWait = 2 * time.Second
)

// EgressSlotBudget은 동시에 송출할 수 있는 egress 수를 제한하고, NVENC
// 세션을 카드에 배분한다. 상한이 필요한 이유는 GPU가 아니라 회선이다 —
// 세션당 상행이 정해져 있어 전부 송출하면 회선이 먼저 포화된다.
//
// 자리는 egress 한 세대의 수명 전체를 점유한다(시작 → Run 종료). 프로세스
// 단위로 잡으면 재연결 때 반납·재획득이 일어나고, 만석이면 그 재연결이
// 실패한다 — 이슈가 "재접속 예비 슬롯"으로 우회하려 한 문제를 점유 구간을
// 늘려 없앤다.
//
// nil *EgressSlotBudget은 유효하며 "제한 없음 + 카드 지정 없음"을 뜻한다.
type EgressSlotBudget struct {
	capacity int
	devices  int

	mu      sync.Mutex
	used    int
	perCard []int
	waiters []chan struct{}
}

// NewEgressSlotBudget은 예산을 만든다. capacity가 0 이하면 총량 제한이 없고,
// devices가 0 이하면 -gpu를 지정하지 않는다. 둘 다 꺼져 있으면 nil을 돌려
// 종전 동작(제한·배정 없음)과 같아진다.
func NewEgressSlotBudget(capacity, devices int) *EgressSlotBudget {
	if capacity <= 0 && devices <= 0 {
		return nil
	}
	budget := &EgressSlotBudget{capacity: capacity, devices: devices}
	if devices > 0 {
		budget.perCard = make([]int, devices)
	}
	return budget
}

// EgressSlot은 획득한 자리다. Release는 여러 번 불러도 안전하다 — 정리 경로가
// 여러 갈래라(정상 종료·조기 실패·세션 종료) 중복 반납이 실제로 일어난다.
type EgressLease struct {
	budget   *EgressSlotBudget
	device   int
	released bool
	mu       sync.Mutex
}

// Device는 이 자리에 배정된 GPU 번호다. 배정이 없으면 -1이다.
func (s *EgressLease) Device() int {
	if s == nil {
		return -1
	}
	return s.device
}

func (s *EgressLease) Release() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return
	}
	s.released = true
	s.budget.release(s.device)
}

// Acquire는 자리를 하나 잡는다. 만석이면 짧게 기다린 뒤 ErrEgressSlotsExhausted를
// 돌려준다. 무한정 기다리면 사용자에게는 멈춘 버튼으로 보이고, 자리는 다른
// 방송이 끝나야 나므로 그 대기는 수십 분이 될 수 있다.
func (b *EgressSlotBudget) Acquire(ctx context.Context) (*EgressLease, error) {
	if b == nil {
		return nil, nil
	}
	deadline := time.NewTimer(egressSlotWait)
	defer deadline.Stop()
	for {
		slot, waiter := b.tryAcquire()
		if slot != nil {
			return slot, nil
		}
		select {
		case <-waiter:
			// 자리가 나면 다시 시도한다. 깨어난 사이 다른 요청이 가져갔을 수
			// 있으므로 획득을 재확인한다.
		case <-deadline.C:
			b.dropWaiter(waiter)
			return nil, ErrEgressSlotsExhausted
		case <-ctx.Done():
			b.dropWaiter(waiter)
			return nil, ctx.Err()
		}
	}
}

// tryAcquire는 즉시 획득을 시도하고, 실패하면 반납 알림을 받을 채널을 준다.
func (b *EgressSlotBudget) tryAcquire() (*EgressLease, chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.capacity > 0 && b.used >= b.capacity {
		return nil, b.appendWaiterLocked()
	}
	device := -1
	if b.devices > 0 {
		device = b.leastLoadedLocked()
		if device < 0 {
			// 총량은 남아도 카드가 전부 한계면 시작할 수 없다.
			return nil, b.appendWaiterLocked()
		}
		b.perCard[device]++
	}
	b.used++
	return &EgressLease{budget: b, device: device}, nil
}

// leastLoadedLocked는 가장 덜 찬 카드를 고른다. 시작 순서만 세는 라운드로빈은
// 종료가 한쪽에 몰리면 편중되어, 총량이 남아도 한 카드가 먼저 한계에 닿는다.
func (b *EgressSlotBudget) leastLoadedLocked() int {
	best := -1
	for index, count := range b.perCard {
		if count >= nvencSessionsPerCard {
			continue
		}
		if best < 0 || count < b.perCard[best] {
			best = index
		}
	}
	return best
}

func (b *EgressSlotBudget) appendWaiterLocked() chan struct{} {
	waiter := make(chan struct{}, 1)
	b.waiters = append(b.waiters, waiter)
	return waiter
}

func (b *EgressSlotBudget) dropWaiter(waiter chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for index, candidate := range b.waiters {
		if candidate == waiter {
			b.waiters = append(b.waiters[:index], b.waiters[index+1:]...)
			return
		}
	}
}

func (b *EgressSlotBudget) release(device int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used > 0 {
		b.used--
	}
	if device >= 0 && device < len(b.perCard) && b.perCard[device] > 0 {
		b.perCard[device]--
	}
	if len(b.waiters) > 0 {
		waiter := b.waiters[0]
		b.waiters = b.waiters[1:]
		select {
		case waiter <- struct{}{}:
		default:
		}
	}
}

// Used는 현재 점유 중인 자리 수다. Capacity는 상한이며 0이면 제한 없음이다.
func (b *EgressSlotBudget) Used() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

func (b *EgressSlotBudget) Capacity() int {
	if b == nil {
		return 0
	}
	return b.capacity
}

// PerCard는 카드별 점유 수 스냅샷이다. 누수 감시가 nvidia-smi 실측과 대조한다.
func (b *EgressSlotBudget) PerCard() []int {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int(nil), b.perCard...)
}
