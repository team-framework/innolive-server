package ai

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// ReadinessSink는 감시 결과를 받는 곳이다. metrics.Registry가 이를 만족한다.
type ReadinessSink interface {
	SetAITargetReady(target string, ready bool)
}

// suspectProbeCooldown은 고장 신호로 같은 워커를 다시 점검하는 최소 간격이다. 한
// 워커에 붙은 세션들이 같은 고장을 동시에 알려도 점검은 한 번만 나간다.
const suspectProbeCooldown = 5 * time.Second

// ReadinessMonitor는 interval마다 모든 AI worker에 preflight 프로브를 보내고
// 결과를 sink와 풀의 준비 상태에 기록한다.
//
// systemd의 active running이나 포트 LISTEN으로는 worker의 런타임 사망을 못 잡는다
// — 프로세스도 소켓도 멀쩡한 채 추론만 죽어 있을 수 있고, 그 상태는 서버가
// 재시작할 때의 기동 preflight에서야 드러났다(#162). 프레임을 실제로 왕복시키는
// 프로브만이 그것을 구분하므로, 기동 때 한 번 하던 검사를 상시로 돌린다.
//
// 풀은 응답 불가로 판정된 워커에 새 세션을 배정하지 않는다(#499). ProcessVideo가
// 워커 고장 신호를 받으면 주기를 기다리지 않고 그 워커만 곧바로 다시 점검한다.
type ReadinessMonitor struct {
	pool        *Pool
	sink        ReadinessSink
	logger      *slog.Logger
	wireFormat  string
	timeout     time.Duration
	interval    time.Duration
	pinLongEdge int

	mu sync.Mutex
	// probing은 지금 점검 중인 대상이다. 같은 대상의 점검은 한 번에 하나만 돈다.
	probing map[string]bool
	// recheck는 점검 중에 고장 신호가 온 대상이다. 돌던 점검은 신호 전에 시작했을
	// 수 있으므로, 끝나면 한 번 더 점검한다.
	recheck map[string]bool
	// suspectedAt은 고장 신호로 대상을 마지막으로 점검한 시각이다.
	suspectedAt map[string]time.Time
}

func NewReadinessMonitor(
	pool *Pool,
	sink ReadinessSink,
	logger *slog.Logger,
	wireFormat string,
	timeout time.Duration,
	interval time.Duration,
	pinLongEdge int,
) *ReadinessMonitor {
	return &ReadinessMonitor{
		pool:        pool,
		sink:        sink,
		logger:      logger,
		wireFormat:  wireFormat,
		timeout:     timeout,
		interval:    interval,
		pinLongEdge: pinLongEdge,
		probing:     make(map[string]bool),
		recheck:     make(map[string]bool),
		suspectedAt: make(map[string]time.Time),
	}
}

// Run은 ctx가 끝날 때까지 interval마다 프로브를 돌린다. 첫 프로브는 기다리지 않고
// 바로 나간다 — 게이지가 첫 주기 동안 빈 채로 남아 있으면 알림이 대상을 못 찾는다.
// 도는 동안 풀의 고장 신호를 받아 해당 워커를 곧바로 다시 점검한다.
func (m *ReadinessMonitor) Run(ctx context.Context) {
	m.pool.SetSuspectHandler(func(client *Client) { m.probeSuspect(ctx, client) })
	defer m.pool.SetSuspectHandler(nil)
	m.probe(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.probe(ctx)
		}
	}
}

// probe는 한 바퀴 돌며 결과를 기록한다. 실패는 진행 중인 방송을 직접 죽이지
// 않는다 — 풀이 새 배정에서 빼고, 그 워커의 세션은 세션 관리자가 옮긴다.
//
// 타깃을 동시에 찔러 각각 끝나는 대로 기록한다. 순차로 돌리면 한 회차가
// timeout×타깃수까지 늘어나 interval을 넘겨버리고(그래서 주기가 설정값보다
// 길어진다), 죽은 워커를 기다리느라 멀쩡한 워커의 시계열까지 늦게 찍힌다 —
// 워커가 죽었을 때 관측이 가장 늦어지는 셈이라 감시의 목적과 반대다.
func (m *ReadinessMonitor) probe(ctx context.Context) {
	var wg sync.WaitGroup
	for _, client := range m.pool.Clients() {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			m.probeTarget(ctx, c)
		}(client)
	}
	wg.Wait()
}

// probeSuspect는 고장 신호를 받은 대상을 곧바로 다시 점검한다. 점검이 이미 돌고
// 있으면 그것이 끝난 뒤 한 번 더 점검한다. suspectProbeCooldown 안에 신호로 점검한
// 대상은 건너뛴다.
func (m *ReadinessMonitor) probeSuspect(ctx context.Context, client *Client) {
	address := client.Address()
	m.mu.Lock()
	if time.Since(m.suspectedAt[address]) < suspectProbeCooldown {
		m.mu.Unlock()
		return
	}
	m.suspectedAt[address] = time.Now()
	if m.probing[address] {
		m.recheck[address] = true
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	go m.probeTarget(ctx, client)
}

// probeTarget은 대상 하나를 점검해 게이지와 풀에 기록한다. 같은 대상의 점검이 이미
// 돌고 있으면 그 결과를 기다리지 않고 건너뛴다. 로그는 준비 상태가 바뀔 때만
// 남긴다 — 죽은 워커를 주기마다 다시 알리지 않는다.
func (m *ReadinessMonitor) probeTarget(ctx context.Context, client *Client) {
	address := client.Address()
	m.mu.Lock()
	if m.probing[address] {
		m.mu.Unlock()
		return
	}
	m.probing[address] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.probing, address)
		recheck := m.recheck[address]
		delete(m.recheck, address)
		m.mu.Unlock()
		if recheck && ctx.Err() == nil {
			go m.probeTarget(ctx, client)
		}
	}()

	callCtx, cancel := context.WithTimeout(ctx, m.timeout)
	err := client.Preflight(callCtx, m.wireFormat, m.pinLongEdge)
	cancel()
	if ctx.Err() != nil {
		// 서버 종료로 끊긴 프로브는 워커 상태가 아니다.
		return
	}

	m.sink.SetAITargetReady(address, err == nil)
	if !m.pool.SetTargetReady(address, err == nil) {
		return
	}
	if err != nil {
		m.logger.Warn(
			"AI worker readiness probe failed; new sessions avoid it",
			"target", address,
			"error", err,
		)
		return
	}
	m.logger.Info("AI worker readiness probe recovered", "target", address)
}
