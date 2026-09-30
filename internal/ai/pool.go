package ai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Pool은 AI 워커 대상마다 클라이언트를 하나씩 두고 세션을 라운드로빈으로
// 배정한다. MPS(또는 소프트웨어 자원 상한)로 워커 프로세스마다 GPU 몫이 정해져
// 있으므로, 세션을 워커에 나눠야 그 격리가 스트림별 격리가 된다.
type Pool struct {
	clients []*Client
	next    atomic.Uint64
	logger  *slog.Logger
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
	return &Pool{clients: clients}, nil
}

// Next는 다음 세션에 쓸 클라이언트를 대상들을 돌아가며 돌려준다.
func (p *Pool) Next() *Client {
	index := (p.next.Add(1) - 1) % uint64(len(p.clients))
	return p.clients[index]
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

// AddWhitelist는 클라이언트의 기준 얼굴을 모든 AI 워커에 등록한다(세션이 대상에
// 라운드로빈으로 퍼지므로 화이트리스트가 전부에 있어야 한다).
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
