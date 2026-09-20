package media

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"inno-live-server/internal/metrics"
)

const (
	// nvencProbeInterval은 nvidia-smi 대조 주기다. 자리 반납은 0.6초 안에
	// 끝나므로(프로덕션 실측) 이 주기에서 보이는 불일치는 전이가 아니다.
	nvencProbeInterval = 30 * time.Second
	// nvencProbeTimeout은 nvidia-smi 한 번의 상한이다. 드라이버가 굳으면
	// nvidia-smi도 같이 굳으므로 감시가 프로브에 매달리지 않게 끊는다.
	nvencProbeTimeout = 5 * time.Second
	// nvencLeakStreak은 누수로 세기까지 필요한 연속 불일치 횟수다. 1회로
	// 세면 종료 직후 프로세스가 아직 살아 있는 창을 누수로 오인한다.
	nvencLeakStreak = 2
)

// NVENCMonitor는 nvidia-smi가 보고하는 NVENC 세션 수와 내부 슬롯 회계를 주기
// 대조한다. 내부 회계 ≥ 실측은 정상이다 — 재연결 구간에는 자리를 쥔 채 NVENC
// 세션이 없다. 위험 신호는 그 반대, 즉 회계에 없는 세션이 카드에 남은 경우다.
type NVENCMonitor struct {
	budget   *EgressSlotBudget
	metrics  *metrics.Registry
	logger   *slog.Logger
	interval time.Duration
	// probe는 카드별 NVENC 세션 수를 돌려준다. 테스트가 nvidia-smi 없이
	// 대조 논리를 검증할 수 있도록 갈라 둔다.
	probe   func(context.Context) ([]int, error)
	streaks []int
}

func NewNVENCMonitor(budget *EgressSlotBudget, registry *metrics.Registry, logger *slog.Logger) *NVENCMonitor {
	return &NVENCMonitor{
		budget:   budget,
		metrics:  registry,
		logger:   logger,
		interval: nvencProbeInterval,
		probe:    queryNVENCSessions,
	}
}

// Run은 ctx가 끝날 때까지 대조를 반복한다. 첫 프로브가 실패하면 nvidia-smi가
// 없는 배포로 보고 조용히 물러난다 — GPU 없는 환경에서 30초마다 경고를 쌓지 않는다.
func (m *NVENCMonitor) Run(ctx context.Context) {
	if err := m.sample(ctx); err != nil {
		if ctx.Err() == nil {
			m.logger.Warn("NVENC leak monitor disabled", "error", err)
		}
		return
	}
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.sample(ctx); err != nil && ctx.Err() == nil {
				m.metrics.IncNVENCProbeFailure()
				m.logger.Warn("NVENC probe failed", "error", err)
			}
		}
	}
}

func (m *NVENCMonitor) sample(ctx context.Context) error {
	measured, err := m.probe(ctx)
	if err != nil {
		return err
	}
	accounted := m.budget.PerCard()
	m.metrics.SetNVENCCards(measured, accounted)
	if len(m.streaks) < len(measured) {
		m.streaks = append(m.streaks, make([]int, len(measured)-len(m.streaks))...)
	}
	for card, sessions := range measured {
		held := 0
		if card < len(accounted) {
			held = accounted[card]
		}
		if sessions <= held {
			m.streaks[card] = 0
			continue
		}
		m.streaks[card]++
		if m.streaks[card] != nvencLeakStreak {
			continue
		}
		m.metrics.IncNVENCLeakSuspected()
		m.logger.Warn("NVENC sessions exceed egress slot accounting",
			"gpu", card, "nvidia_smi_sessions", sessions, "accounted_slots", held)
	}
	return nil
}

// queryNVENCSessions는 카드별 인코더 세션 수를 읽는다. 값은 카드 전체의 것이라
// 우리 FFmpeg 말고 다른 프로세스가 인코딩하면 그만큼 높게 나온다 —
// 이 서버가 GPU를 독점한다는 전제 위에서만 대조가 성립한다.
func queryNVENCSessions(ctx context.Context) ([]int, error) {
	probeCtx, cancel := context.WithTimeout(ctx, nvencProbeTimeout)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, "nvidia-smi",
		"--query-gpu=encoder.stats.sessionCount", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, fmt.Errorf("run nvidia-smi: %w", err)
	}
	var sessions []int
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		count, err := strconv.Atoi(line)
		if err != nil {
			return nil, fmt.Errorf("parse nvidia-smi output %q: %w", line, err)
		}
		sessions = append(sessions, count)
	}
	if len(sessions) == 0 {
		return nil, fmt.Errorf("nvidia-smi reported no GPUs")
	}
	return sessions, nil
}
