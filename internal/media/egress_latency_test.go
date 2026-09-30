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
	// 1ms..100ms 지연 표본 100개를 순서 없이 넣는다.
	for _, ms := range []int{50, 1, 99, 100, 2, 95, 3} {
		tracker.observe(base.Add(-time.Duration(ms) * time.Millisecond))
	}
	// 백분위 인덱스를 두루 쓰도록 값을 흩어 총 100개로 채운다.
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
	// 측정 지연은 주입한 차이에 base 이후 흐른 벽시계를 더한 값이라 정확한 값이
	// 아니라 범위로 검증한다. 이 테스트가 보는 것은 측정 정밀도가 아니라 백분위
	// 선택이다. 부하가 걸린 러너는 observe() 100번 동안 수 ms를 밀리므로 허용 폭을
	// 넉넉히 둔다.
	const toleranceMs = 25.0
	for _, want := range []struct {
		key   string
		floor float64
	}{
		{"p50_ms", 51},  // 51st smallest of the injected 1..100ms
		{"p95_ms", 96},  // 96th smallest
		{"min_ms", 1},   // 가장 작은 값
		{"max_ms", 100}, // 가장 큰 값
	} {
		got := logFloat(t, out, want.key)
		if got < want.floor || got >= want.floor+toleranceMs {
			t.Errorf("%s = %v, want in [%v, %v): %s", want.key, got, want.floor, want.floor+toleranceMs, out)
		}
	}
}

// logFloat는 slog 텍스트 한 줄에서 float 속성 하나를 읽는다.
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

	// 꺼져 있으면 flush해도 로그를 남기지 않는다.
	disabled := newLatencyTracker(logger, false)
	disabled.observe(time.Now().Add(-time.Second))
	disabled.flush()

	// 켜져 있어도 수신 타임스탬프가 0이면 무시한다(예: 하네스 프레임).
	enabled := newLatencyTracker(logger, true)
	enabled.observe(time.Time{})
	enabled.flush()

	if buf.Len() != 0 {
		t.Fatalf("expected no output, got: %s", buf.String())
	}
}
