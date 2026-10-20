package ai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Pool은 AI 워커 대상마다 클라이언트를 하나씩 두고 세션을 활성 스트림이 가장
// 적은 대상에 배정한다. MPS(또는 소프트웨어 자원 상한)로 워커 프로세스마다 GPU
// 몫이 정해져 있으므로, 세션을 워커에 나눠야 그 격리가 스트림별 격리가 된다.
type Pool struct {
	clients []*Client
	logger  *slog.Logger

	mu sync.Mutex
	// active는 대상별로 지금 열려 있는 영상 파이프라인 수다.
	active []int
	// next는 활성 수가 같은 대상 사이에서 시작 위치를 돌려 고르게 퍼뜨린다.
	next int
	// observer는 대상별 활성 수가 바뀔 때마다 불린다(메트릭 게이지).
	observer func(target string, active int)
	// unready는 상태 점검이 응답 불가로 판정한 대상이다. 준비된 대상이 하나라도
	// 있으면 Acquire는 여기 든 대상을 고르지 않는다(#499). 비어 있으면 모두 준비된
	// 것으로 본다 — 기동 preflight를 지난 상태가 출발점이다.
	unready map[string]bool
	// readinessObserver는 대상의 준비 상태가 바뀔 때만 불린다.
	readinessObserver func(target string, ready bool)
	// suspectHandler는 ProcessVideo가 워커 고장 신호를 받았을 때 불린다. 상시
	// 점검이 그 대상을 주기를 기다리지 않고 다시 점검하도록 연결한다.
	suspectHandler func(*Client)
	// faceSwap은 얼굴 합성 세션만 받는 대상이다(#507). 비어 있으면 모든 대상이 두
	// 모드를 받는다. 비어 있지 않으면 합성 세션은 여기 든 대상에만, 블러 세션은
	// 나머지 대상에만 간다 — 합성 옵션 없이 띄운 워커는 합성 요청을 받으면 모델을
	// CPU로 지연 로드해 그 워커의 블러 세션까지 멈춘다.
	faceSwap map[string]bool
}

// SetFaceSwapTargets는 얼굴 합성 세션만 받을 대상을 정한다(#507). 세션을 배정하기
// 전에 부른다. 비우면 모든 대상이 두 모드를 받는다. 모르는 대상이 있거나 블러
// 세션을 받을 대상이 하나도 남지 않으면 거절한다.
func (p *Pool) SetFaceSwapTargets(targets []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	reserved := make(map[string]bool, len(targets))
	for _, target := range targets {
		if !p.hasTargetLocked(target) {
			return fmt.Errorf("face swap target %q is not in the AI target list", target)
		}
		reserved[target] = true
	}
	if len(reserved) > 0 && len(reserved) == len(p.clients) {
		return errors.New("face swap targets must leave at least one AI target for blur sessions")
	}
	p.faceSwap = reserved
	return nil
}

// servesLocked는 대상이 이 모드의 세션을 받는지다. p.mu를 쥔 채 불린다.
func (p *Pool) servesLocked(target string, faceSwap bool) bool {
	return len(p.faceSwap) == 0 || p.faceSwap[target] == faceSwap
}

// SetStreamObserver는 대상별 활성 스트림 수가 바뀔 때마다 부를 함수를 붙인다.
// 지금 값을 한 번씩 먼저 알려 게이지가 0부터 보이게 한다.
func (p *Pool) SetStreamObserver(observer func(target string, active int)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observer = observer
	if observer == nil {
		return
	}
	for index, client := range p.clients {
		observer(client.Address(), p.active[index])
	}
}

// SetLogger는 로거를 붙인다. 화이트리스트 롤백처럼 최선 노력 경로가 재시도를 다
// 한 실패를 남길 수 있게 한다. nil 로거도 유효하며 아무것도 남기지 않는다.
func (p *Pool) SetLogger(logger *slog.Logger) {
	if p != nil {
		p.logger = logger
	}
}

// RetryWhitelistDelete는 retryWhitelistDelete의 공개 형태다. 이 패키지 밖의
// 호출자(자기 등록을 되돌리는 핸들러)도 보상 삭제에 같은 재시도 정책을 쓰게 한다.
func RetryWhitelistDelete(ctx context.Context, del func(context.Context) error) error {
	return retryWhitelistDelete(ctx, del)
}

// whitelistDeleteAttempts는 보상 화이트리스트 삭제를 포기하기 전까지 시도하는
// 횟수다. 잠깐 내려갔거나 멈춘 워커는 대개 이 안에 회복하므로, 일시적 실패로
// 오래된 엔트리가 남지 않는다.
const whitelistDeleteAttempts = 3

// retryWhitelistDelete는 보상 화이트리스트 삭제를 조금씩 늘어나는 간격으로 몇 번
// 시도하고 처음 성공하면 멈춘다. 조용히 실패한 롤백은 그 롤백이 지우려던 바로 그
// 엔트리를 남기기 때문에 있다. 컨텍스트가 취소되면 일찍 멈춘다.
func retryWhitelistDelete(ctx context.Context, del func(context.Context) error) error {
	backoff := 100 * time.Millisecond
	var err error
	for attempt := 0; attempt < whitelistDeleteAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff *= 2
		}
		if err = del(ctx); err == nil {
			return nil
		}
	}
	return err
}

func NewPool(targets []string, timeout time.Duration) (*Pool, error) {
	if len(targets) == 0 {
		return nil, errors.New("AI gRPC target list is empty")
	}
	clients := make([]*Client, 0, len(targets))
	for _, target := range targets {
		client, err := New(target, timeout)
		if err != nil {
			for _, created := range clients {
				_ = created.Close()
			}
			return nil, fmt.Errorf("create AI client for %q: %w", target, err)
		}
		clients = append(clients, client)
	}
	pool := &Pool{clients: clients, active: make([]int, len(clients))}
	for _, client := range clients {
		client.onSuspect = pool.reportSuspect
	}
	return pool, nil
}

// Acquire는 활성 스트림이 가장 적은 준비된 대상을 골라 스트림 하나를 배정하고, 그
// 배정을 되돌리는 release를 함께 돌려준다. 호출자는 파이프라인이 끝날 때 release를
// 한 번 불러야 한다(여러 번 불러도 한 번만 반영된다).
//
// 워커 한 프로세스는 프레임을 차례로 처리하므로 같은 워커에 몰린 스트림 수가 곧
// 프레임 지연이다(#477). 종전 라운드로빈은 끝난 세션·파이프라인 재시작에도
// 카운터가 넘어가, 동시 2세션이 한 워커에 몰리고 다른 워커가 놀 수 있었다.
//
// 상태 점검이 응답 불가로 판정한 대상은 고르지 않는다(#499). 런타임이 죽은 워커는
// 첫 프레임부터 실패해 세션이 블러로 잠긴다. 준비된 대상이 하나도 없으면 전체에서
// 고른다 — 세션은 어차피 fail-closed로 막히고, 워커가 돌아오면 그대로 이어 쓴다.
//
// 합성 전용 대상이 정해져 있으면 faceSwap 모드의 대상 안에서만 고른다(#507).
// 준비된 대상이 없을 때도 다른 모드의 대상으로 넘기지 않는다.
func (p *Pool) Acquire(faceSwap bool) (*Client, func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	skipUnready := p.hasReadyTargetLocked("", faceSwap)
	best := -1
	for offset := 0; offset < len(p.clients); offset++ {
		index := (p.next + offset) % len(p.clients)
		address := p.clients[index].Address()
		if !p.servesLocked(address, faceSwap) {
			continue
		}
		if skipUnready && p.unready[address] {
			continue
		}
		if best < 0 || p.active[index] < p.active[best] {
			best = index
		}
	}
	p.next = (best + 1) % len(p.clients)
	p.adjust(best, 1)
	var once sync.Once
	release := func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.adjust(best, -1)
		})
	}
	return p.clients[best], release
}

// adjust는 p.mu를 쥔 채 불린다.
func (p *Pool) adjust(index, delta int) {
	p.active[index] += delta
	if p.observer != nil {
		p.observer(p.clients[index].Address(), p.active[index])
	}
}

// SetReadinessObserver는 대상의 준비 상태가 바뀔 때마다 부를 함수를 붙인다. 응답
// 불가가 된 워커에 붙은 세션을 다른 워커로 옮기는 데 쓴다(#499). 호출은 p.mu 밖에서
// 일어나므로 observer가 풀을 다시 불러도 된다.
func (p *Pool) SetReadinessObserver(observer func(target string, ready bool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readinessObserver = observer
}

// SetTargetReady는 상태 점검 결과로 대상의 준비 상태를 기록하고, 상태가 바뀌었는지
// 돌려준다. 바뀐 때만 readinessObserver를 부른다. 모르는 대상은 무시한다.
func (p *Pool) SetTargetReady(target string, ready bool) bool {
	p.mu.Lock()
	if !p.hasTargetLocked(target) || p.unready[target] == !ready {
		p.mu.Unlock()
		return false
	}
	if ready {
		delete(p.unready, target)
	} else {
		if p.unready == nil {
			p.unready = make(map[string]bool)
		}
		p.unready[target] = true
	}
	observer := p.readinessObserver
	p.mu.Unlock()
	if observer != nil {
		observer(target, ready)
	}
	return true
}

// TargetReady는 대상이 마지막 상태 점검에서 응답 불가로 판정되지 않았는지다.
func (p *Pool) TargetReady(target string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.unready[target]
}

// HasReadyTargetOtherThan은 target과 같은 모드를 받는 대상 중 target 말고 준비된
// 대상이 있는지다. 응답 불가 워커의 세션을 옮겨도 갈 곳이 있는지 판단한다. 세션은
// 자기 모드의 대상에만 배정되므로 target의 모드가 곧 세션의 모드다(#507).
func (p *Pool) HasReadyTargetOtherThan(target string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hasReadyTargetLocked(target, p.faceSwap[target])
}

// HasReadyTarget은 faceSwap 모드의 세션을 받을 준비된 대상이 있는지다. 합성 세션을
// 만들기 전에 합성 워커가 살아 있는지 확인한다(#507).
func (p *Pool) HasReadyTarget(faceSwap bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hasReadyTargetLocked("", faceSwap)
}

// hasReadyTargetLocked는 faceSwap 모드를 받는 대상 중 except 말고 준비된 대상이
// 있는지다. p.mu를 쥔 채 불린다.
func (p *Pool) hasReadyTargetLocked(except string, faceSwap bool) bool {
	for _, client := range p.clients {
		address := client.Address()
		if address != except && p.servesLocked(address, faceSwap) && !p.unready[address] {
			return true
		}
	}
	return false
}

func (p *Pool) hasTargetLocked(target string) bool {
	for _, client := range p.clients {
		if client.Address() == target {
			return true
		}
	}
	return false
}

// SetSuspectHandler는 ProcessVideo가 워커 고장 신호를 받았을 때 부를 함수를 붙인다.
// nil이면 신호를 버린다.
func (p *Pool) SetSuspectHandler(handler func(*Client)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.suspectHandler = handler
}

// reportSuspect는 클라이언트가 워커 고장 신호를 받았다고 알린다.
func (p *Pool) reportSuspect(client *Client) {
	p.mu.Lock()
	handler := p.suspectHandler
	p.mu.Unlock()
	if handler != nil {
		handler(client)
	}
}

func (p *Pool) Clients() []*Client {
	result := make([]*Client, len(p.clients))
	copy(result, p.clients)
	return result
}

func (p *Pool) Close() error {
	var errs []error
	for _, client := range p.clients {
		if err := client.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// WhitelistResult는 기준 얼굴 하나를 풀 전체에 등록한 결과다. 대표 워커 응답과
// 워커마다 발급한 엔트리 id를 담는다.
type WhitelistResult struct {
	Response *aiv1.WhitelistResponse
	// EntryIDs는 워커 주소별로 그 워커가 이 얼굴에 발급한 엔트리 id다. 워커마다
	// id를 따로 만들므로(uuid4) 같은 얼굴도 워커마다 id가 다르고, 나중에 지우려면
	// 워커마다 자기 id를 보내야 한다 — DeleteWhitelistEntries 참고.
	EntryIDs map[string]string
}

// partialAddRollbackTimeout은 아래 보상 삭제의 한계다. 브로드캐스트가 실패할 때
// 요청 컨텍스트는 이미 취소된 경우가 많아, 롤백은 분리해 돌리고 자기 기한을 둔다.
const partialAddRollbackTimeout = 5 * time.Second

// AddWhitelist는 클라이언트의 기준 얼굴을 모든 AI 워커에 등록한다(세션이 어느
// 대상에 배정될지 미리 알 수 없으므로 화이트리스트가 전부에 있어야 한다).
//
// 오류가 나면 얼굴은 어디에도 등록되지 않은 상태다. 받아들인 워커는 여기서
// 되돌린다. 호출자는 이를 할 수 없다 — 엔트리 id는 워커마다 발급되고 부분 성공의
// id는 이 브로드캐스트만 본다. 오류를 돌려주면서 그 워커들이 얼굴을 계속 블러에서
// 제외하게 두면 영영 남는다.
func (p *Pool) AddWhitelist(ctx context.Context, sessionID string, data []byte) (*WhitelistResult, error) {
	result, err := p.broadcast("add", func(c *Client) (*aiv1.WhitelistResponse, error) {
		return c.AddWhitelist(ctx, sessionID, data)
	})
	if err != nil {
		if result != nil && len(result.EntryIDs) > 0 {
			p.rollbackPartialAdd(ctx, sessionID, result.EntryIDs)
		}
		return nil, err
	}
	return result, nil
}

// rollbackPartialAdd는 실패한 AddWhitelist가 그래도 등록해 버린 엔트리를 지운다.
// 최선 노력이다 — 호출자가 이미 실패를 알리고 있으므로, 삭제마저 실패하면 워커
// 쪽 로그에만 남고 여기서는 버린다.
func (p *Pool) rollbackPartialAdd(ctx context.Context, sessionID string, entryIDs map[string]string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), partialAddRollbackTimeout)
	defer cancel()
	err := retryWhitelistDelete(ctx, func(c context.Context) error {
		return p.DeleteWhitelistEntries(c, sessionID, entryIDs)
	})
	if err != nil && p.logger != nil {
		p.logger.Error("partial whitelist add rollback failed after retries; stale worker entries may remain",
			"session_id", sessionID, "error", err)
	}
}

// DeleteWhitelistEntries는 등록한 얼굴 하나를 그 얼굴을 가진 모든 워커에서 지운다.
// 워커마다 그 워커가 발급한 엔트리 id로 요청한다(entryIDs는 AddWhitelist의 맵).
// 맵에 없는 워커는 얼굴을 저장한 적이 없어 건너뛴다. id를 더는 모르는 워커(재시작
// 했거나 이미 지워진 엔트리)는 지운 것으로 쳐서, 얼굴이 영영 못 지우는 상태가 되지
// 않게 한다.
func (p *Pool) DeleteWhitelistEntries(ctx context.Context, sessionID string, entryIDs map[string]string) error {
	return p.runOnWorkers("delete", func(c *Client) error {
		entryID := entryIDs[c.Address()]
		if entryID == "" {
			return nil
		}
		_, err := c.DeleteWhitelist(ctx, sessionID, entryID)
		return ignoreNotFound(err)
	})
}

// ClearWhitelist는 각 워커가 이 세션에 대해 지금 가진 엔트리를 모두 지운다. 엔트리
// id는 이 서버의 기록이 아니라 워커에서 받아 오므로, 둘이 어긋났어도 워커
// 화이트리스트는 비게 된다. 워커가 빈 엔트리 id를 거절해 "전부 지우기" 호출 하나로
// 대신할 수 없다.
func (p *Pool) ClearWhitelist(ctx context.Context, sessionID string) error {
	return p.runOnWorkers("clear", func(c *Client) error {
		snapshot, err := c.GetWhitelistStatus(ctx, sessionID)
		if err != nil {
			return ignoreNotFound(err)
		}
		for _, entryID := range snapshot.GetEntryIds() {
			if _, err := c.DeleteWhitelist(ctx, sessionID, entryID); err != nil {
				if err = ignoreNotFound(err); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// whitelistStatusTimeout은 WhitelistEntryIDs의 호출 하나의 한계다. 상태 조회는
// 모델을 거치지 않는 메타데이터 읽기라, 이보다 오래 걸리는 워커는 멈춘 것으로 본다.
const whitelistStatusTimeout = 5 * time.Second

// WhitelistEntryIDs는 워커마다 각 세션에 대해 지금 가진 화이트리스트 엔트리 id를
// 읽는다. 결과는 워커 주소 → 세션 → 엔트리 id이고 모든 워커 주소를 담는다.
//
// 답을 받지 못한 세션은 그 워커의 맵에 없다. 호출자는 그것을 "모름"으로 다뤄야
// 한다 — 엔트리가 없다고 보면 잠깐 끊긴 워커 하나가 모든 얼굴을 잃은 것처럼 보인다.
// 워커가 모르는 세션(NotFound)은 엔트리가 없는 것으로 친다. 한 워커에서 오류가
// 나면 그 워커의 남은 세션은 묻지 않는다 — 멈춘 워커가 세션 수만큼 타임아웃을
// 쌓지 않게 한다.
func (p *Pool) WhitelistEntryIDs(ctx context.Context, sessionIDs []string) map[string]map[string][]string {
	type answer struct {
		address string
		entries map[string][]string
	}
	answers := make(chan answer, len(p.clients))
	for _, client := range p.clients {
		go func(c *Client) {
			entries := make(map[string][]string, len(sessionIDs))
			for _, sessionID := range sessionIDs {
				callCtx, cancel := context.WithTimeout(ctx, whitelistStatusTimeout)
				snapshot, err := c.GetWhitelistStatus(callCtx, sessionID)
				cancel()
				if ignoreNotFound(err) != nil {
					break
				}
				entries[sessionID] = append([]string{}, snapshot.GetEntryIds()...)
			}
			answers <- answer{address: c.Address(), entries: entries}
		}(client)
	}
	result := make(map[string]map[string][]string, len(p.clients))
	for range p.clients {
		worker := <-answers
		result[worker.address] = worker.entries
	}
	return result
}

// ignoreNotFound는 워커가 없는 엔트리·세션에 대해 돌려준 오류를 버린다. 호출자는
// 그것이 없어지길 원했고, 없다.
func ignoreNotFound(err error) error {
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}

// broadcast는 op를 모든 워커에 동시에 실행한다. 부분 실패는 실패한 대상을 밝혀
// 알리고(호출자가 재시도할 수 있다 — 워커는 다시 적용해도 멱등이다), 워커 쪽
// 거절("failed" 상태)은 돌려주는 응답으로 드러낸다. 부분 실패면 결과와 오류를 함께
// 돌려줘 호출자가 성공한 부분으로 처리할 수 있게 한다.
func (p *Pool) broadcast(kind string, op func(*Client) (*aiv1.WhitelistResponse, error)) (*WhitelistResult, error) {
	type outcome struct {
		address  string
		response *aiv1.WhitelistResponse
		err      error
	}
	results := make(chan outcome, len(p.clients))
	for _, client := range p.clients {
		go func(c *Client) {
			response, err := op(c)
			results <- outcome{address: c.Address(), response: response, err: err}
		}(client)
	}

	result := &WhitelistResult{EntryIDs: make(map[string]string, len(p.clients))}
	var errs []error
	for range p.clients {
		worker := <-results
		if worker.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", worker.address, worker.err))
			continue
		}
		if entryID := worker.response.GetEntryId(); entryID != "" {
			result.EntryIDs[worker.address] = entryID
		}
		if result.Response == nil || worker.response.GetStatusMessage() == "failed" {
			result.Response = worker.response
		}
	}
	if len(errs) > 0 {
		// 부분 결과를 오류와 함께 넘긴다. 성공한 워커의 엔트리 id는 다른 어디에도
		// 없고, AddWhitelist가 부분 등록을 되돌리려면 그것이 필요하다.
		return result, fmt.Errorf("whitelist %s broadcast failed on %d/%d targets: %w", kind, len(errs), len(p.clients), errors.Join(errs...))
	}
	return result, nil
}

// runOnWorkers는 op를 모든 워커에 동시에 실행하고, 실패를 낸 대상과 함께 모은다.
func (p *Pool) runOnWorkers(kind string, op func(*Client) error) error {
	results := make(chan error, len(p.clients))
	for _, client := range p.clients {
		go func(c *Client) {
			if err := op(c); err != nil {
				results <- fmt.Errorf("%s: %w", c.Address(), err)
				return
			}
			results <- nil
		}(client)
	}
	var errs []error
	for range p.clients {
		if err := <-results; err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("whitelist %s failed on %d/%d targets: %w", kind, len(errs), len(p.clients), errors.Join(errs...))
	}
	return nil
}
