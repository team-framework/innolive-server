package ai

import (
	"context"
	"fmt"
	"maps"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
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

// countingAIServer mimics the real worker's whitelist bookkeeping: every entry
// id is minted by this worker alone, so two workers never agree on the id for
// the same face, and an unknown id is rejected with NOT_FOUND.
type countingAIServer struct {
	aiv1.UnimplementedAiProcessorServer
	name           string
	whitelistCalls atomic.Int32

	mu      sync.Mutex
	entries []string
	nextID  int
}

func (s *countingAIServer) AddWhitelist(context.Context, *aiv1.FaceData) (*aiv1.WhitelistResponse, error) {
	s.whitelistCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	entryID := fmt.Sprintf("%s-entry-%d", s.name, s.nextID)
	s.entries = append(s.entries, entryID)
	return &aiv1.WhitelistResponse{StatusMessage: "success", Timestamp: 1, EntryId: entryID}, nil
}

func (s *countingAIServer) DeleteWhitelist(_ context.Context, request *aiv1.DeleteWhitelistRequest) (*aiv1.WhitelistResponse, error) {
	if strings.TrimSpace(request.GetEntryId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "entry_id must not be empty or whitespace-only")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	remaining := make([]string, 0, len(s.entries))
	for _, entry := range s.entries {
		if entry != request.GetEntryId() {
			remaining = append(remaining, entry)
		}
	}
	if len(remaining) == len(s.entries) {
		return nil, status.Errorf(codes.NotFound, "whitelist entry %q does not exist", request.GetEntryId())
	}
	s.entries = remaining
	return &aiv1.WhitelistResponse{StatusMessage: "success", Timestamp: 1, EntryId: request.GetEntryId()}, nil
}

func (s *countingAIServer) GetWhitelistStatus(context.Context, *aiv1.GetWhitelistStatusRequest) (*aiv1.GetWhitelistStatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &aiv1.GetWhitelistStatusResponse{EntryCount: uint32(len(s.entries)), EntryIds: append([]string(nil), s.entries...)}, nil
}

func (s *countingAIServer) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.entries...)
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
	serverA := &countingAIServer{name: "a"}
	serverB := &countingAIServer{name: "b"}
	pool := &Pool{clients: []*Client{
		newBufconnClient(t, "worker-a", serverA),
		newBufconnClient(t, "worker-b", serverB),
	}}

	result, err := pool.AddWhitelist(context.Background(), "session", []byte("face"))
	if err != nil {
		t.Fatalf("AddWhitelist() error = %v", err)
	}
	if result.Response.GetStatusMessage() != "success" {
		t.Fatalf("status = %q, want success", result.Response.GetStatusMessage())
	}
	// Each worker mints its own id; keeping only one of them is what made
	// per-face deletion fail against a multi-worker pool.
	want := map[string]string{"worker-a": "a-entry-1", "worker-b": "b-entry-1"}
	if !maps.Equal(result.EntryIDs, want) {
		t.Fatalf("EntryIDs = %v, want %v", result.EntryIDs, want)
	}
	if serverA.whitelistCalls.Load() != 1 || serverB.whitelistCalls.Load() != 1 {
		t.Fatalf("whitelist calls = (%d, %d), want (1, 1) — broadcast must reach every worker",
			serverA.whitelistCalls.Load(), serverB.whitelistCalls.Load())
	}
}

func TestPoolDeleteWhitelistEntriesUsesEachWorkersOwnEntryID(t *testing.T) {
	serverA := &countingAIServer{name: "a"}
	serverB := &countingAIServer{name: "b"}
	pool := &Pool{clients: []*Client{
		newBufconnClient(t, "worker-a", serverA),
		newBufconnClient(t, "worker-b", serverB),
	}}
	ctx := context.Background()

	first, err := pool.AddWhitelist(ctx, "session", []byte("face-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.AddWhitelist(ctx, "session", []byte("face-2")); err != nil {
		t.Fatal(err)
	}

	if err := pool.DeleteWhitelistEntries(ctx, "session", first.EntryIDs); err != nil {
		t.Fatalf("DeleteWhitelistEntries() error = %v", err)
	}
	if got := serverA.snapshot(); len(got) != 1 || got[0] != "a-entry-2" {
		t.Fatalf("worker-a entries = %v, want only a-entry-2", got)
	}
	if got := serverB.snapshot(); len(got) != 1 || got[0] != "b-entry-2" {
		t.Fatalf("worker-b entries = %v, want only b-entry-2", got)
	}
}

func TestPoolDeleteWhitelistEntriesTreatsUnknownEntryAsDeleted(t *testing.T) {
	server := &countingAIServer{name: "a"}
	pool := &Pool{clients: []*Client{newBufconnClient(t, "worker-a", server)}}

	// A worker that restarted no longer knows the entry; the face is gone either
	// way, so the caller must not be blocked from dropping its own record.
	err := pool.DeleteWhitelistEntries(context.Background(), "session", map[string]string{"worker-a": "a-entry-1"})
	if err != nil {
		t.Fatalf("DeleteWhitelistEntries() on an unknown entry = %v, want nil", err)
	}
}

func TestPoolClearWhitelistDeletesEveryEntryTheWorkerHolds(t *testing.T) {
	serverA := &countingAIServer{name: "a"}
	serverB := &countingAIServer{name: "b"}
	pool := &Pool{clients: []*Client{
		newBufconnClient(t, "worker-a", serverA),
		newBufconnClient(t, "worker-b", serverB),
	}}
	ctx := context.Background()
	for range 3 {
		if _, err := pool.AddWhitelist(ctx, "session", []byte("face")); err != nil {
			t.Fatal(err)
		}
	}

	// The worker rejects an empty entry id, so clearing must enumerate the ids
	// the worker itself reports rather than send one "delete all" request.
	if err := pool.ClearWhitelist(ctx, "session"); err != nil {
		t.Fatalf("ClearWhitelist() error = %v", err)
	}
	if got := serverA.snapshot(); len(got) != 0 {
		t.Fatalf("worker-a entries = %v, want empty", got)
	}
	if got := serverB.snapshot(); len(got) != 0 {
		t.Fatalf("worker-b entries = %v, want empty", got)
	}
}

func TestPoolAddWhitelistReportsFailedTarget(t *testing.T) {
	healthy := &countingAIServer{name: "a"}
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
