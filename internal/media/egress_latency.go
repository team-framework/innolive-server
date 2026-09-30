package media

import (
	"log/slog"
	"sort"
	"time"
)

// egressLatencyWindow는 추적기가 모은 표본을 요약하는 주기다.
const egressLatencyWindow = 10 * time.Second

// latencyTracker는 수신→egress 쓰기 프레임 지연을 주기적인 p50/p95/max 로그 줄로
// 요약한다. A/V 싱크 오프셋을 정하려는 4-1단계 측정 도구다. 영상 경로는 블러 왕복만큼
// 늦고 오디오는 그대로 지나가므로, 이 지연이 대략 오디오가 앞서는 정도다.
//
// egress 쓰기 고루틴에서만 건드리므로 동기화가 필요 없다. 창 번호를 붙여 지연이
// 고정 오프셋인지 시간에 따라 흐르는지 읽는 쪽이 볼 수 있다.
type latencyTracker struct {
	logger      *slog.Logger
	enabled     bool
	samples     []time.Duration
	windowStart time.Time
	windowIdx   int
}

func newLatencyTracker(logger *slog.Logger, enabled bool) *latencyTracker {
	return &latencyTracker{logger: logger, enabled: enabled}
}

// observe는 전달한 프레임 하나의 수신→쓰기 지연을 기록하고, 창이 지나면 요약을
// 내보낸다. 수신 타임스탬프가 없는 프레임(예: 테스트 하네스 프레임)은 무시한다.
func (l *latencyTracker) observe(ingestAt time.Time) {
	if !l.enabled || ingestAt.IsZero() {
		return
	}
	now := time.Now()
	if l.windowStart.IsZero() {
		l.windowStart = now
	}
	l.samples = append(l.samples, now.Sub(ingestAt))
	if now.Sub(l.windowStart) >= egressLatencyWindow {
		l.flush()
	}
}

func (l *latencyTracker) flush() {
	l.windowStart = time.Now()
	if len(l.samples) == 0 {
		return
	}
	sort.Slice(l.samples, func(i, j int) bool { return l.samples[i] < l.samples[j] })
	n := len(l.samples)
	l.logger.Info("egress ingest→write latency",
		"window", l.windowIdx,
		"frames", n,
		"p50_ms", milliseconds(l.samples[n*50/100]),
		"p95_ms", milliseconds(l.samples[min(n*95/100, n-1)]),
		"min_ms", milliseconds(l.samples[0]),
		"max_ms", milliseconds(l.samples[n-1]),
	)
	l.samples = l.samples[:0]
	l.windowIdx++
}

func milliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}
