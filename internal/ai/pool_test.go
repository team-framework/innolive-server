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
	// grpc.NewClient는 지연 연결이라 닿지 않는 대상도 여기서는 괜찮다.
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

// countingAIServer는 실제 워커의 화이트리스트 기록을 흉내 낸다. 엔트리 id는 이
// 워커만 발급하므로 두 워커가 같은 얼굴에 같은 id를 쓰는 일이 없고, 모르는 id는
// NOT_FOUND로 거절한다.
type countingAIServer struct {
	aiv1.UnimplementedAiProcessorServer
	name           string
	whitelistCalls atomic.Int32

	// deleteFailsLeft: >0이면 그만큼의 DeleteWhitelist 호출을 Unavailable로
	// 떨어뜨린 뒤 정상 동작한다. 재시도가 일시적 실패를 넘기는지 검증용.
	deleteFailsLeft atomic.Int32

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
	if s.deleteFailsLeft.Load() > 0 {
		s.deleteFailsLeft.Add(-1)
		return nil, status.Error(codes.Unavailable, "worker temporarily down")
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
	// 워커마다 자기 id를 발급한다. 하나만 남기면 다중 워커 풀에서 얼굴별 삭제가
	// 실패했다.
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

	// 재시작한 워커는 엔트리를 모른다. 어느 쪽이든 얼굴은 없으므로 호출자가 자기
	// 기록을 지우는 것을 막으면 안 된다.
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

	// 워커는 빈 엔트리 id를 거절하므로, 비우기는 "전부 지우기" 요청 하나가 아니라
	// 워커가 알려 준 id를 하나씩 지워야 한다.
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

// 한 워커에서 실패한 브로드캐스트는 받아들인 워커에 얼굴을 남기면 안 된다. 그
// 엔트리 id는 브로드캐스트만 보므로, "등록 실패"를 받은 호출자는 나중에 지울
// 방법이 없다(#205).
func TestPoolAddWhitelistRollsBackPartialRegistration(t *testing.T) {
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

	if _, err := pool.AddWhitelist(context.Background(), "session", []byte("face")); err == nil {
		t.Fatal("AddWhitelist() with an unreachable worker should fail")
	}
	if entries := healthy.snapshot(); len(entries) != 0 {
		t.Fatalf("healthy worker entries = %v, want empty — a failed add must register nowhere", entries)
	}
}

// 롤백은 취소된 요청 컨텍스트에서도 살아야 한다. 업로드 도중 끊긴 클라이언트가
// 컨텍스트를 취소하고, 그 취소가 브로드캐스트가 실패하는 경로 중 하나다.
func TestPoolRollbackPartialAddIgnoresCancelledContext(t *testing.T) {
	healthy := &countingAIServer{name: "a"}
	pool := &Pool{clients: []*Client{newBufconnClient(t, "worker-a", healthy)}}

	result, err := pool.AddWhitelist(context.Background(), "session", []byte("face"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	pool.rollbackPartialAdd(ctx, "session", result.EntryIDs)

	if entries := healthy.snapshot(); len(entries) != 0 {
		t.Fatalf("worker entries = %v, want empty — rollback must not ride the cancelled context", entries)
	}
}

func TestRetryWhitelistDeleteSucceedsAfterTransientFailures(t *testing.T) {
	calls := 0
	err := RetryWhitelistDelete(context.Background(), func(context.Context) error {
		calls++
		if calls < 3 {
			return status.Error(codes.Unavailable, "down")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RetryWhitelistDelete() = %v, want nil after recovery", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (two failures then success)", calls)
	}
}

func TestRetryWhitelistDeleteReturnsLastErrorWhenAllFail(t *testing.T) {
	calls := 0
	sentinel := status.Error(codes.Unavailable, "still down")
	err := RetryWhitelistDelete(context.Background(), func(context.Context) error {
		calls++
		return sentinel
	})
	if err == nil {
		t.Fatal("RetryWhitelistDelete() = nil, want the last error after exhausting retries")
	}
	if calls != whitelistDeleteAttempts {
		t.Fatalf("calls = %d, want %d", calls, whitelistDeleteAttempts)
	}
}

func TestRetryWhitelistDeleteStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := RetryWhitelistDelete(ctx, func(context.Context) error {
		calls++
		return status.Error(codes.Unavailable, "down")
	})
	if err == nil {
		t.Fatal("want context error")
	}
	// 첫 시도는 돌고, 취소된 컨텍스트가 backoff 대기를 멈춘다.
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 before the cancelled backoff", calls)
	}
}

// 보상 삭제가 처음에 실패한 부분 AddWhitelist도 워커가 회복하면 롤백 재시도 덕에
// 엔트리를 지워야 한다(#215).
func TestPoolRollbackPartialAddRetriesTransientDeleteFailure(t *testing.T) {
	healthy := &countingAIServer{name: "a"}
	healthy.deleteFailsLeft.Store(2) // 롤백 삭제 두 번은 실패하고 세 번째에 성공
	unreachable, err := New("127.0.0.1:1", 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	pool := &Pool{clients: []*Client{
		newBufconnClient(t, "worker-a", healthy),
		unreachable,
	}}
	defer unreachable.Close()

	if _, err := pool.AddWhitelist(context.Background(), "session", []byte("face")); err == nil {
		t.Fatal("AddWhitelist() with an unreachable worker should fail")
	}
	if entries := healthy.snapshot(); len(entries) != 0 {
		t.Fatalf("healthy worker entries = %v, want empty after retried rollback", entries)
	}
}
