package ai

import (
	"context"
	"log/slog"
	"time"
)

// ReadinessSink는 감시 결과를 받는 곳이다. metrics.Registry가 이를 만족한다.
type ReadinessSink interface {
	SetAITargetReady(target string, ready bool)
}

// ReadinessMonitor는 interval마다 모든 AI worker에 preflight 프로브를 보내고
// 결과를 sink에 기록한다.
//
// systemd의 active running이나 포트 LISTEN으로는 worker의 런타임 사망을 못 잡는다
// — 프로세스도 소켓도 멀쩡한 채 추론만 죽어 있을 수 있고, 그 상태는 서버가
// 재시작할 때의 기동 preflight에서야 드러났다(#162). 프레임을 실제로 왕복시키는
// 프로브만이 그것을 구분하므로, 기동 때 한 번 하던 검사를 상시로 돌린다.
type ReadinessMonitor struct {
	pool        *Pool
	sink        ReadinessSink
	logger      *slog.Logger
	wireFormat  string
	timeout     time.Duration
	interval    time.Duration
	pinLongEdge int
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
	}
}

// Run은 ctx가 끝날 때까지 interval마다 프로브를 돌린다. 첫 프로브는 기다리지 않고
// 바로 나간다 — 게이지가 첫 주기 동안 빈 채로 남아 있으면 알림이 대상을 못 찾는다.
func (m *ReadinessMonitor) Run(ctx context.Context) {
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

// probe는 한 바퀴 돌며 결과를 기록한다. 실패는 로그와 게이지로만 남는다 —
// 진행 중인 방송을 죽이지 않기 위해서다. 대응은 게이지를 보는 알림의 몫이다.
func (m *ReadinessMonitor) probe(ctx context.Context) {
	for _, result := range m.pool.PreflightTargets(ctx, m.wireFormat, m.timeout, m.pinLongEdge) {
		m.sink.SetAITargetReady(result.Target, result.Err == nil)
		if result.Err != nil {
			m.logger.Warn(
				"AI worker readiness probe failed",
				"target", result.Target,
				"error", result.Err,
			)
		}
	}
}
