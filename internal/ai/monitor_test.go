package ai

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// readySpy는 감시가 기록한 target별 최신 판정을 모은다.
type readySpy struct {
	mu      sync.Mutex
	states  map[string]bool
	updates int
	changed chan struct{}
}

func newReadySpy() *readySpy {
	return &readySpy{states: make(map[string]bool), changed: make(chan struct{}, 64)}
}

func (s *readySpy) SetAITargetReady(target string, ready bool) {
	s.mu.Lock()
	s.states[target] = ready
	s.updates++
	s.mu.Unlock()
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *readySpy) snapshot() (map[string]bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	states := make(map[string]bool, len(s.states))
	for target, ready := range s.states {
		states[target] = ready
	}
	return states, s.updates
}

// waitForUpdates는 최소 want건의 기록이 쌓일 때까지 기다린다.
func (s *readySpy) waitForUpdates(t *testing.T, want int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if _, updates := s.snapshot(); updates >= want {
			return
		}
		select {
		case <-s.changed:
		case <-deadline:
			_, updates := s.snapshot()
			t.Fatalf("readiness updates = %d, want at least %d", updates, want)
		}
	}
}

func testMonitor(pool *Pool, sink ReadinessSink, interval time.Duration) *ReadinessMonitor {
	return NewReadinessMonitor(
		pool,
		sink,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"jpeg",
		time.Second,
		interval,
		0,
	)
}

// TestReadinessMonitorRecordsPerTargetState: 추론이 되는 worker와 죽은 worker가
// 섞여 있을 때 target별로 갈라 기록해야 한다. 이 구분이 #162의 핵심이다 —
// 한 worker만 죽어도 다른 worker는 멀쩡히 돌기 때문에 합쳐진 신호로는 못 잡는다.
func TestReadinessMonitorRecordsPerTargetState(t *testing.T) {
	healthy := preflightClient(t, echoAIServer{})
	healthy.address = "healthy:50051"
	broken := preflightClient(t, badStatusServer{})
	broken.address = "broken:50052"

	spy := newReadySpy()
	monitor := testMonitor(&Pool{clients: []*Client{healthy, broken}}, spy, time.Hour)
	monitor.probe(context.Background())

	states, _ := spy.snapshot()
	if !states["healthy:50051"] {
		t.Errorf("healthy target ready = false, want true")
	}
	if states["broken:50052"] {
		t.Errorf("broken target ready = true, want false")
	}
}

// TestReadinessMonitorRunProbesRepeatedly: 기동 시점 한 번이 아니라 상시로
// 돌아야 하고(그래서 첫 프로브도 주기를 기다리지 않는다), ctx가 끝나면
// 고루틴이 남지 않아야 한다.
func TestReadinessMonitorRunProbesRepeatedly(t *testing.T) {
	client := preflightClient(t, echoAIServer{})
	client.address = "healthy:50051"

	spy := newReadySpy()
	monitor := testMonitor(&Pool{clients: []*Client{client}}, spy, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		monitor.Run(ctx)
		close(done)
	}()

	spy.waitForUpdates(t, 3)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after the context was cancelled")
	}
}
