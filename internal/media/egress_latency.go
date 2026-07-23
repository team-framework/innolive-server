package media

import (
	"log/slog"
	"sort"
	"time"
)

// egressLatencyWindow is how often the tracker summarises collected samples.
const egressLatencyWindow = 10 * time.Second

// latencyTracker summarises ingest→egress-write frame latency into periodic
// p50/p95/max log lines. It is Phase 4-1 measurement scaffolding used to size
// the A/V sync offset: the video path is delayed by the blur round-trip while
// audio passes through, so this delay is roughly how far ahead the audio runs.
//
// It is only touched from the egress writer goroutine, so it needs no
// synchronisation. A per-window index lets a reader see whether the delay is a
// stable offset or drifts over time.
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

// observe records one delivered frame's ingest→write latency and flushes a
// summary once a window elapses. Frames without an ingest timestamp (e.g. test
// harness frames) are ignored.
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
