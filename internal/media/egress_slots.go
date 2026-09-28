package media

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrEgressSlotsExhausted는 송출 자리가 없어 egress를 시작할 수 없는 경우다.
// 자리는 다른 방송이 끝나야 나므로 즉시 재시도해도 풀리지 않는다 — 호출자는
// 대기가 아니라 사용자에게 알릴 실패로 다뤄야 한다.
var ErrEgressSlotsExhausted = errors.New("no egress slot is available")

// ErrEgressCappedUnitsExhausted는 상한 그룹(Spark)의 몫이 찬 경우다. 총량은
// 남아 있을 수 있다. 기존 만석 처리와 같게 다루도록 ErrEgressSlotsExhausted로도
// 판정된다.
var ErrEgressCappedUnitsExhausted = fmt.Errorf("%w: capped group is full", ErrEgressSlotsExhausted)

// EgressClaim은 자리를 잡는 송출 하나의 회계 정보다(#272). media는 플랜을
// 모른다 — 서버·세션 계층이 해석해 넘긴다.
type EgressClaim struct {
	// Owner는 유닛을 합산하는 단위(세션)다. 비어 있으면 이 송출 하나가 독립
	// 소유자다.
	Owner string
	// HighRes는 FHD 송출이다. 유닛은 720p 송출 n개가 n, FHD 송출 n개가 n+1이다.
	HighRes bool
	// Capped는 상한 그룹(Spark)에 속하는지다.
	Capped bool
	// Group은 메트릭용 이름(플랜)이다.
	Group string
}

// egressUnits는 한 소유자가 count개 송출할 때의 유닛이다. 720p 한 곳 1 · FHD 한 곳 2 ·
// 720p 동시 2 · FHD 동시 3 — 노션 BM의 차감 배수와 같다.
func egressUnits(highRes bool, count int) int {
	if count <= 0 {
		return 0
	}
	if highRes {
		return count + 1
	}
	return count
}

type egressOwner struct {
	claim EgressClaim
	count int
	held  int
}

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
	// cappedLimit는 상한 그룹이 함께 쓸 수 있는 유닛이다. 0이면 상한이 없다.
	// 하위 플랜을 막는 것만으로 상위 플랜 몫이 지켜진다.
	cappedLimit int

	mu          sync.Mutex
	used        int
	cappedUsed  int
	perCard     []int
	owners      map[string]*egressOwner
	anonymous   int
	waiters     []chan struct{}
	usedByGroup map[string]int
}

// NewEgressSlotBudget은 예산을 만든다. capacity가 0 이하면 총량 제한이 없고,
// devices가 0 이하면 -gpu를 지정하지 않는다. 둘 다 꺼져 있으면 nil을 돌려
// 종전 동작(제한·배정 없음)과 같아진다.
func NewEgressSlotBudget(capacity, devices int) *EgressSlotBudget {
	if capacity <= 0 && devices <= 0 {
		return nil
	}
	budget := &EgressSlotBudget{capacity: capacity, devices: devices, owners: map[string]*egressOwner{}, usedByGroup: map[string]int{}}
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
	owner    string
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
	s.budget.release(s.device, s.owner)
}

// Acquire는 자리를 하나 잡는다. 만석이면 짧게 기다린 뒤 ErrEgressSlotsExhausted를
// 돌려준다. 무한정 기다리면 사용자에게는 멈춘 버튼으로 보이고, 자리는 다른
// 방송이 끝나야 나므로 그 대기는 수십 분이 될 수 있다.
func (b *EgressSlotBudget) Acquire(ctx context.Context) (*EgressLease, error) {
	return b.AcquireClaim(ctx, EgressClaim{})
}

// AcquireClaim은 유닛 회계를 적용해 자리를 잡는다(#272). 같은 Owner의 송출이
// 늘면 그 소유자의 유닛을 다시 계산해 차이만큼만 더 잡는다 — FHD 동시 송출이
// 2+2가 아니라 3이 되는 것이 이 때문이다.
func (b *EgressSlotBudget) AcquireClaim(ctx context.Context, claim EgressClaim) (*EgressLease, error) {
	if b == nil {
		return nil, nil
	}
	deadline := time.NewTimer(egressSlotWait)
	defer deadline.Stop()
	for {
		slot, waiter, reason := b.tryAcquire(claim)
		if slot != nil {
			return slot, nil
		}
		select {
		case <-waiter:
			// 자리가 나면 다시 시도한다. 깨어난 사이 다른 요청이 가져갔을 수
			// 있으므로 획득을 재확인한다.
		case <-deadline.C:
			b.abandonWaiter(waiter)
			return nil, reason
		case <-ctx.Done():
			b.abandonWaiter(waiter)
			return nil, ctx.Err()
		}
	}
}

// tryAcquire는 즉시 획득을 시도하고, 실패하면 반납 알림을 받을 채널과 실패
// 사유를 준다.
func (b *EgressSlotBudget) tryAcquire(claim EgressClaim) (*EgressLease, chan struct{}, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	owner := b.owners[claim.Owner]
	if claim.Owner == "" || owner == nil {
		owner = &egressOwner{claim: claim}
	}
	delta := egressUnits(owner.claim.HighRes, owner.count+1) - owner.held
	if b.capacity > 0 && b.used+delta > b.capacity {
		return nil, b.appendWaiterLocked(), ErrEgressSlotsExhausted
	}
	if owner.claim.Capped && b.cappedLimit > 0 && b.cappedUsed+delta > b.cappedLimit {
		return nil, b.appendWaiterLocked(), ErrEgressCappedUnitsExhausted
	}
	device := -1
	if b.devices > 0 {
		device = b.leastLoadedLocked()
		if device < 0 {
			// 총량은 남아도 카드가 전부 한계면 시작할 수 없다.
			return nil, b.appendWaiterLocked(), ErrEgressSlotsExhausted
		}
		b.perCard[device]++
	}
	owner.count++
	owner.held += delta
	b.used += delta
	if owner.claim.Capped {
		b.cappedUsed += delta
	}
	b.usedByGroup[owner.claim.Group] += delta
	key := claim.Owner
	if key == "" {
		b.anonymous++
		key = fmt.Sprintf("\x00anonymous-%d", b.anonymous)
	}
	b.owners[key] = owner
	return &EgressLease{budget: b, device: device, owner: key}, nil, nil
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

// abandonWaiter는 기다리기를 포기한 요청을 대기열에서 뺀다. 이미 대기열에서
// 빠졌다면 반납 신호가 이 요청에게 전달된 뒤 시간이 다한 것이므로, 그 신호를
// 다음 대기자에게 넘긴다 — 그러지 않으면 자리가 비어 있는데도 남은 대기자가
// 자기 시간이 다할 때까지 기다리다 실패한다.
func (b *EgressSlotBudget) abandonWaiter(waiter chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for index, candidate := range b.waiters {
		if candidate == waiter {
			b.waiters = append(b.waiters[:index], b.waiters[index+1:]...)
			return
		}
	}
	// 대기열에 없다 = 이미 신호를 받았다. 받은 신호를 흘리지 않고 넘긴다.
	select {
	case <-waiter:
		b.wakeOneLocked()
	default:
	}
}

// release는 송출 하나를 반납하고, 소유자의 남은 송출 수로 유닛을 다시 계산해
// 차이만 돌려준다 — FHD 동시 송출에서 한쪽이 먼저 끝나면 3에서 1이 아니라 2가
// 남는다.
func (b *EgressSlotBudget) release(device int, key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if owner := b.owners[key]; owner != nil {
		owner.count--
		freed := owner.held - egressUnits(owner.claim.HighRes, owner.count)
		owner.held -= freed
		b.used -= freed
		if owner.claim.Capped {
			b.cappedUsed -= freed
		}
		b.usedByGroup[owner.claim.Group] -= freed
		if b.usedByGroup[owner.claim.Group] <= 0 {
			delete(b.usedByGroup, owner.claim.Group)
		}
		if owner.count <= 0 {
			delete(b.owners, key)
		}
	}
	if device >= 0 && device < len(b.perCard) && b.perCard[device] > 0 {
		b.perCard[device]--
	}
	// 유닛 크기가 달라 먼저 깨운 대기자가 못 들어갈 수 있으므로 전부 깨운다.
	for len(b.waiters) > 0 {
		b.wakeOneLocked()
	}
}

// wakeOneLocked는 대기자 하나에게 자리가 났음을 알린다. 호출자가 mu를 쥐고 있어야 한다.
func (b *EgressSlotBudget) wakeOneLocked() {
	if len(b.waiters) == 0 {
		return
	}
	waiter := b.waiters[0]
	b.waiters = b.waiters[1:]
	select {
	case waiter <- struct{}{}:
	default:
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

// SetCappedLimit는 상한 그룹(Spark)이 함께 쓸 수 있는 유닛을 정한다. 0이면
// 상한이 없다. 서버 조립 단계에서 한 번 부른다.
func (b *EgressSlotBudget) SetCappedLimit(units int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cappedLimit = units
}

// UsedByGroup은 그룹(플랜)별 점유 유닛 스냅샷이다.
func (b *EgressSlotBudget) UsedByGroup() map[string]int {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	snapshot := make(map[string]int, len(b.usedByGroup))
	for group, units := range b.usedByGroup {
		snapshot[group] = units
	}
	return snapshot
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
