package media

import (
	"bytes"
	"log/slog"
	"strconv"
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
	// Each measured latency is the injected delta plus the wall clock elapsed
	// since base, so assert a range instead of an exact value: what this test
	// checks is percentile selection, not measurement precision. The tolerance
	// is generous because a loaded runner adds several ms of drift over 100
	// observe() calls.
	const toleranceMs = 25.0
	for _, want := range []struct {
		key   string
		floor float64
	}{
		{"p50_ms", 51},  // 51st smallest of the injected 1..100ms
		{"p95_ms", 96},  // 96th smallest
		{"min_ms", 1},   // smallest
		{"max_ms", 100}, // largest
	} {
		got := logFloat(t, out, want.key)
		if got < want.floor || got >= want.floor+toleranceMs {
			t.Errorf("%s = %v, want in [%v, %v): %s", want.key, got, want.floor, want.floor+toleranceMs, out)
		}
	}
}

// logFloat reads one float attribute out of a slog text line.
func logFloat(t *testing.T, line, key string) float64 {
	t.Helper()
	for _, field := range strings.Fields(line) {
		name, value, ok := strings.Cut(field, "=")
		if !ok || name != key {
			continue
		}
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("parsing %s: %v", field, err)
		}
		return f
	}
	t.Fatalf("no %s in log line: %s", key, line)
	return 0
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
