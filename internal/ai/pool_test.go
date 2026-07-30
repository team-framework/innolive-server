package ai

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestPoolNextCyclesRoundRobin(t *testing.T) {
	// grpc.NewClient dials lazily, so unreachable targets are fine here.
	pool, err := NewPool([]string{"a:1", "b:1", "c:1"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	want := []string{"a:1", "b:1", "c:1", "a:1", "b:1", "c:1"}
	for index, expected := range want {
		if got := pool.Next().Address(); got != expected {
			t.Fatalf("Next() #%d = %q, want %q", index, got, expected)
		}
	}
	if clients := pool.Clients(); len(clients) != 3 {
		t.Fatalf("Clients() length = %d, want 3", len(clients))
	}
}

func TestNewPoolRejectsEmptyTargetList(t *testing.T) {
	if _, err := NewPool(nil, time.Second); err == nil {
		t.Fatal("NewPool(nil) should fail")
	}
}

type countingAIServer struct {
	aiv1.UnimplementedAiProcessorServer
	whitelistCalls atomic.Int32
}

func (s *countingAIServer) AddWhitelist(context.Context, *aiv1.FaceData) (*aiv1.WhitelistResponse, error) {
	s.whitelistCalls.Add(1)
	return &aiv1.WhitelistResponse{StatusMessage: "success", Timestamp: 1}, nil
}

func newBufconnClient(t *testing.T, address string, server *countingAIServer) *Client {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	aiv1.RegisterAiProcessorServer(grpcServer, server)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)

	connection, err := grpc.NewClient(
		"passthrough:///"+address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{address: address, conn: connection, client: aiv1.NewAiProcessorClient(connection), timeout: time.Second}
}

func TestPoolAddWhitelistBroadcastsToEveryWorker(t *testing.T) {
	serverA := &countingAIServer{}
	serverB := &countingAIServer{}
	pool := &Pool{clients: []*Client{
		newBufconnClient(t, "worker-a", serverA),
		newBufconnClient(t, "worker-b", serverB),
	}}

	response, err := pool.AddWhitelist(context.Background(), "", []byte("face"))
	if err != nil {
		t.Fatalf("AddWhitelist() error = %v", err)
	}
	if response.GetStatusMessage() != "success" {
		t.Fatalf("status = %q, want success", response.GetStatusMessage())
	}
	if serverA.whitelistCalls.Load() != 1 || serverB.whitelistCalls.Load() != 1 {
		t.Fatalf("whitelist calls = (%d, %d), want (1, 1) — broadcast must reach every worker",
			serverA.whitelistCalls.Load(), serverB.whitelistCalls.Load())
	}
}

func TestPoolAddWhitelistReportsFailedTarget(t *testing.T) {
	healthy := &countingAIServer{}
	unreachable, err := New("127.0.0.1:1", 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	pool := &Pool{clients: []*Client{
		newBufconnClient(t, "worker-a", healthy),
		unreachable,
	}}
	defer unreachable.Close()

	if _, err := pool.AddWhitelist(context.Background(), "", []byte("face")); err == nil {
		t.Fatal("AddWhitelist() with an unreachable worker should fail")
	} else if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("error should name the failed target, got: %v", err)
	}
	if healthy.whitelistCalls.Load() != 1 {
		t.Fatalf("healthy worker calls = %d, want 1 (broadcast must still attempt all)", healthy.whitelistCalls.Load())
	}
}
