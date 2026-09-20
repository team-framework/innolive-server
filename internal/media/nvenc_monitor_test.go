package media

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"inno-live-server/internal/metrics"
)

func newTestNVENCMonitor(budget *EgressSlotBudget, probe func(context.Context) ([]int, error)) (*NVENCMonitor, *metrics.Registry) {
	registry := metrics.New()
	monitor := NewNVENCMonitor(budget, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	monitor.probe = probe
	return monitor, registry
}

func metricValue(t *testing.T, registry *metrics.Registry, name string) string {
	t.Helper()
	var out strings.Builder
	registry.WritePrometheus(&out)
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, name+" ") {
			return strings.TrimPrefix(line, name+" ")
		}
	}
	t.Fatalf("metric %s not found in:\n%s", name, out.String())
	return ""
}

// 내부 회계가 실측보다 많은 것은 정상이다 — 재연결 구간에는 자리를 쥔 채
// NVENC 세션이 없다. 이 방향을 누수로 세면 매 재연결이 경보가 된다.
func TestNVENCMonitorIgnoresAccountedExcess(t *testing.T) {
	budget := NewEgressSlotBudget(4, 2)
	lease, err := budget.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()

	monitor, registry := newTestNVENCMonitor(budget, func(context.Context) ([]int, error) {
		return []int{0, 0}, nil
	})
	for range nvencLeakStreak + 1 {
		if err := monitor.sample(context.Background()); err != nil {
			t.Fatalf("sample: %v", err)
		}
	}
	if got := metricValue(t, registry, "innolive_nvenc_leak_suspected_total"); got != "0" {
		t.Fatalf("leak suspected = %s, want 0", got)
	}
	if !strings.Contains(metricsText(registry), `innolive_nvenc_slots_accounted{gpu="0"} 1`) {
		t.Fatalf("accounted gauge missing:\n%s", metricsText(registry))
	}
}

// 회계에 없는 세션이 카드에 남은 것이 누수 신호다. 다만 종료 직후 한 창은
// 전이일 수 있으므로 연속 2회부터 센다.
func TestNVENCMonitorCountsUnaccountedSessions(t *testing.T) {
	budget := NewEgressSlotBudget(4, 2)
	monitor, registry := newTestNVENCMonitor(budget, func(context.Context) ([]int, error) {
		return []int{1, 0}, nil
	})
	if err := monitor.sample(context.Background()); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if got := metricValue(t, registry, "innolive_nvenc_leak_suspected_total"); got != "0" {
		t.Fatalf("after one probe leak suspected = %s, want 0", got)
	}
	if err := monitor.sample(context.Background()); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if got := metricValue(t, registry, "innolive_nvenc_leak_suspected_total"); got != "1" {
		t.Fatalf("after two probes leak suspected = %s, want 1", got)
	}
	if !strings.Contains(metricsText(registry), `innolive_nvenc_sessions{gpu="0"} 1`) {
		t.Fatalf("measured gauge missing:\n%s", metricsText(registry))
	}
}

// 불일치가 끊기면 연속 횟수도 끊긴다 — 오래된 전이가 쌓여 나중에 오경보가 되면 안 된다.
func TestNVENCMonitorResetsStreakOnMatch(t *testing.T) {
	budget := NewEgressSlotBudget(4, 2)
	sessions := 1
	monitor, registry := newTestNVENCMonitor(budget, func(context.Context) ([]int, error) {
		return []int{sessions, 0}, nil
	})
	if err := monitor.sample(context.Background()); err != nil {
		t.Fatalf("sample: %v", err)
	}
	sessions = 0
	if err := monitor.sample(context.Background()); err != nil {
		t.Fatalf("sample: %v", err)
	}
	sessions = 1
	if err := monitor.sample(context.Background()); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if got := metricValue(t, registry, "innolive_nvenc_leak_suspected_total"); got != "0" {
		t.Fatalf("leak suspected = %s, want 0", got)
	}
}

// nvidia-smi가 없는 배포에서는 첫 프로브 실패로 감시가 물러난다. 30초마다
// 경고를 쌓지 않으면서도 Run이 반환하는지 확인한다.
func TestNVENCMonitorStopsWhenFirstProbeFails(t *testing.T) {
	probes := 0
	monitor, registry := newTestNVENCMonitor(NewEgressSlotBudget(0, 2), func(context.Context) ([]int, error) {
		probes++
		return nil, errors.New("nvidia-smi not found")
	})
	monitor.interval = time.Millisecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		monitor.Run(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after the first probe failed")
	}
	if probes != 1 {
		t.Fatalf("probes = %d, want 1", probes)
	}
	if got := metricValue(t, registry, "innolive_nvenc_probe_failures_total"); got != "0" {
		t.Fatalf("probe failures = %s, want 0 (startup failure is not a probe failure)", got)
	}
}

// 기동 후의 프로브 실패는 감시를 멈추지 않지만 보이게 남긴다 — 감시가 죽으면
// 불일치가 0으로 보인다.
func TestNVENCMonitorCountsLaterProbeFailures(t *testing.T) {
	probes := 0
	monitor, registry := newTestNVENCMonitor(NewEgressSlotBudget(0, 1), func(context.Context) ([]int, error) {
		probes++
		if probes == 1 {
			return []int{0}, nil
		}
		return nil, errors.New("driver busy")
	})
	monitor.interval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		monitor.Run(ctx)
	}()
	deadline := time.After(2 * time.Second)
	for metricValue(t, registry, "innolive_nvenc_probe_failures_total") == "0" {
		select {
		case <-deadline:
			t.Fatal("probe failure was never recorded")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func metricsText(registry *metrics.Registry) string {
	var out strings.Builder
	registry.WritePrometheus(&out)
	return out.String()
}
