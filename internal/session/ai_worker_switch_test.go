package session

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/ai"
	"inno-live-server/internal/metrics"

	"google.golang.org/grpc"
)

type echoWorker struct {
	aiv1.UnimplementedAiProcessorServer
}

func (echoWorker) ProcessVideo(stream grpc.BidiStreamingServer[aiv1.VideoChunk, aiv1.ProcessedVideoChunk]) error {
	for {
		request, err := stream.Recv()
		if err != nil {
			return nil
		}
		if err := stream.Send(&aiv1.ProcessedVideoChunk{Data: request.GetData(), Timestamp: request.GetTimestamp(), StatusMessage: "success"}); err != nil {
			return err
		}
	}
}

func startEchoWorker(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	aiv1.RegisterAiProcessorServer(server, echoWorker{})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

// 워커를 비우면 그 워커의 세션이 파이프라인 재시작 없이 다른 워커로 넘어가고, 배정
// 수도 함께 옮겨 간다(#515).
func TestSwitchSessionsOffAIWorkerMovesStreamsAndAssignments(t *testing.T) {
	from, to := startEchoWorker(t), startEchoWorker(t)
	pool, err := ai.NewPool([]string{from, to}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := pool.SetDraining(to, true); err != nil {
		t.Fatal(err)
	}
	client, release := pool.Acquire(false)
	if client.Address() != from {
		t.Fatalf("Acquire() = %q, want %q", client.Address(), from)
	}
	stream := ai.NewSwitchableStream(context.Background(), client, release, "client-1", false)
	t.Cleanup(stream.Release)
	if _, err := stream.Process([]byte("frame"), 1, 0, 0, "", nil); err != nil {
		t.Fatalf("Process() error = %v", err)
	}

	moving := &Session{ID: "moving", aiTarget: from, aiStream: stream}
	m := &Manager{
		ai:      pool,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics: metrics.New(),
		sessions: map[string]*Session{
			"moving":   moving,
			"no-video": {ID: "no-video", aiTarget: from},
		},
	}

	// 갈 곳이 없으면 옮기지 않고 사유를 남긴다.
	if report := m.SwitchSessionsOffAIWorker(context.Background(), from); report.Moved != 0 || report.Failed["moving"] == "" {
		t.Fatalf("report with no open worker = %+v, want moving failed", report)
	}

	if err := pool.SetDraining(to, false); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetDraining(from, true); err != nil {
		t.Fatal(err)
	}
	report := m.SwitchSessionsOffAIWorker(context.Background(), from)
	if report.Moved != 1 || len(report.Failed) != 0 {
		t.Fatalf("report = %+v, want one session moved", report)
	}
	if moving.aiTarget != to || stream.Target() != to {
		t.Fatalf("session target = %q stream target = %q, want %q", moving.aiTarget, stream.Target(), to)
	}
	for _, target := range pool.Targets() {
		want := map[string]int{from: 0, to: 1}[target.Address]
		if target.Active != want {
			t.Fatalf("target %s active = %d, want %d", target.Address, target.Active, want)
		}
	}
	if response, err := stream.Process([]byte("frame"), 2, 0, 0, "", nil); err != nil || response.GetTimestamp() != 2 {
		t.Fatalf("Process() after move = %v, %v", response, err)
	}
}
