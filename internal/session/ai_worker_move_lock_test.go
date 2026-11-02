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

// silentWorker는 프레임을 받기만 하고 답하지 않는다. 새 워커 예열이 끝나지 않는다.
type silentWorker struct {
	aiv1.UnimplementedAiProcessorServer
}

func (silentWorker) ProcessVideo(stream grpc.BidiStreamingServer[aiv1.VideoChunk, aiv1.ProcessedVideoChunk]) error {
	for {
		if _, err := stream.Recv(); err != nil {
			return nil
		}
	}
}

// 워커 이동은 새 워커 예열을 최대 20초 기다린다. 그동안 해상도 잠금을 쥐면 해상도
// 전환이 막혀 송출 공백이 길어진다(P2-11). 스트림 전환은 파이프라인을 다시 띄우지
// 않으므로 잠금이 필요한 것은 실패 뒤 재시작뿐이다.
func TestAIWorkerMoveDoesNotHoldResolutionLockWhileWarmingUp(t *testing.T) {
	from := startEchoWorker(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	aiv1.RegisterAiProcessorServer(server, silentWorker{})
	go server.Serve(listener)
	to := listener.Addr().String()

	pool, err := ai.NewPool([]string{from, to}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := pool.SetDraining(to, true); err != nil {
		t.Fatal(err)
	}
	client, release := pool.Acquire(false)
	stream := ai.NewSwitchableStream(context.Background(), client, release, "client-1", false)
	t.Cleanup(stream.Release)
	if _, err := stream.Process([]byte("frame"), 1, 0, 0, "", nil); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if err := pool.SetDraining(to, false); err != nil {
		t.Fatal(err)
	}

	moving := &Session{ID: "moving", aiTarget: from, aiStream: stream}
	m := &Manager{
		ai:       pool,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics:  metrics.New(),
		sessions: map[string]*Session{"moving": moving},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.moveVideoPipelineOffAIWorker(moving, from)
	}()
	t.Cleanup(func() {
		server.Stop()
		<-done
	})

	time.Sleep(300 * time.Millisecond)
	if !moving.resolutionMu.TryLock() {
		t.Fatal("resolution lock is held while the new AI worker warms up")
	}
	moving.resolutionMu.Unlock()
}
