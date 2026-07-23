package media

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLatencyTrackerPercentiles(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	tracker := newLatencyTracker(logger, true)

	base := time.Now()
	// 100 samples of 1ms..100ms latency, fed out of order.
	for _, ms := range []int{50, 1, 99, 100, 2, 95, 3} {
		tracker.observe(base.Add(-time.Duration(ms) * time.Millisecond))
	}
	// Fill to 100 total with a spread so percentile indices are exercised.
	for ms := 4; ms <= 98; ms++ {
		if ms == 50 || ms == 95 || ms == 99 {
			continue
		}
		tracker.observe(base.Add(-time.Duration(ms) * time.Millisecond))
	}
	tracker.flush()

	out := buf.String()
	if !strings.Contains(out, "frames=100") {
		t.Fatalf("expected 100 frames, got: %s", out)
	}
	// p50≈50ms, p95≈95ms, min≈1ms, max≈100ms (each measured latency is at least
	// the injected delta; the tiny wall-clock elapsed since base keeps it above).
	for _, want := range []string{"p50_ms=5", "p95_ms=9", "min_ms=1", "max_ms=10"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in log line: %s", want, out)
		}
	}
}

func TestLatencyTrackerDisabledAndZero(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	// Disabled: never logs even when flushed.
	disabled := newLatencyTracker(logger, false)
	disabled.observe(time.Now().Add(-time.Second))
	disabled.flush()

	// Enabled but zero ingest timestamps are ignored (e.g. harness frames).
	enabled := newLatencyTracker(logger, true)
	enabled.observe(time.Time{})
	enabled.flush()

	if buf.Len() != 0 {
		t.Fatalf("expected no output, got: %s", buf.String())
	}
}
