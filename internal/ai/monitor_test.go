package ai

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc"
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

// hangingServer는 프레임을 받고 아무것도 돌려주지 않는다. 프로세스와 소켓은
// 살아 있는데 런타임만 죽어 프로브가 타임아웃까지 매달리는 worker를 대신한다 —
// #162에서 실제로 관측된 상태다.
type hangingServer struct {
	aiv1.UnimplementedAiProcessorServer
}

func (hangingServer) ProcessVideo(stream grpc.BidiStreamingServer[aiv1.VideoChunk, aiv1.ProcessedVideoChunk]) error {
	<-stream.Context().Done()
	return stream.Context().Err()
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

// TestReadinessMonitorProbesTargetsConcurrently: 매달린 worker가 섞여 있어도
// 한 회차는 타임아웃 하나만큼만 걸려야 하고, 멀쩡한 worker의 판정은 죽은
// worker를 기다리지 않고 먼저 기록돼야 한다. 순차로 돌리면 회차가
// 타임아웃×타깃수로 늘어나 interval을 넘기고, 워커가 죽었을 때 관측이 가장
// 늦어진다.
func TestReadinessMonitorProbesTargetsConcurrently(t *testing.T) {
	healthy := preflightClient(t, echoAIServer{})
	healthy.address = "healthy:50051"
	hung := preflightClient(t, hangingServer{})
	hung.address = "hung:50052"
	alsoHung := preflightClient(t, hangingServer{})
	alsoHung.address = "also-hung:50053"

	spy := newReadySpy()
	monitor := testMonitor(&Pool{clients: []*Client{hung, alsoHung, healthy}}, spy, time.Hour)
	const timeout = 300 * time.Millisecond
	monitor.timeout = timeout

	start := time.Now()
	monitor.probe(context.Background())
	elapsed := time.Since(start)

	// 순차라면 타임아웃 2회(매달린 worker 2대)를 합쳐 최소 2×timeout이 걸린다.
	if elapsed >= 2*timeout {
		t.Errorf("probe round took %s, want well under %s — targets are not probed concurrently", elapsed, 2*timeout)
	}

	states, updates := spy.snapshot()
	if updates != 3 {
		t.Errorf("readiness updates = %d, want 3 (one per target)", updates)
	}
	if !states["healthy:50051"] {
		t.Error("healthy target ready = false, want true")
	}
	for _, dead := range []string{"hung:50052", "also-hung:50053"} {
		if states[dead] {
			t.Errorf("%s ready = true, want false", dead)
		}
	}
}
