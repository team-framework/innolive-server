package ai

import (
	"context"
	"net"
	"slices"
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

// statusAIServer는 GetWhitelistStatus만 답한다. 세션마다 정해 둔 엔트리를 돌려주고,
// 없는 세션에는 err를 돌려준다.
type statusAIServer struct {
	aiv1.UnimplementedAiProcessorServer
	entries map[string][]string
	err     error
	calls   atomic.Int32
}

func (s *statusAIServer) GetWhitelistStatus(_ context.Context, request *aiv1.GetWhitelistStatusRequest) (*aiv1.GetWhitelistStatusResponse, error) {
	s.calls.Add(1)
	entries, ok := s.entries[request.GetSessionId()]
	if !ok && s.err != nil {
		return nil, s.err
	}
	return &aiv1.GetWhitelistStatusResponse{SessionId: request.GetSessionId(), EntryIds: entries}, nil
}

func newStatusClient(t *testing.T, address string, server *statusAIServer) *Client {
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

func TestPoolWhitelistEntryIDsReadsEveryWorkerPerSession(t *testing.T) {
	holding := &statusAIServer{entries: map[string][]string{"user:a": {"a-1", "a-2"}, "user:b": {}}}
	// 재시작한 워커는 세션을 모른다. 워커 버전에 따라 빈 목록이나 NotFound로 답한다.
	restarted := &statusAIServer{err: status.Error(codes.NotFound, "session does not exist")}
	pool := &Pool{clients: []*Client{
		newStatusClient(t, "worker-a", holding),
		newStatusClient(t, "worker-b", restarted),
	}}

	got := pool.WhitelistEntryIDs(context.Background(), []string{"user:a", "user:b"})

	if entries, ok := got["worker-a"]["user:a"]; !ok || !slices.Equal(entries, []string{"a-1", "a-2"}) {
		t.Fatalf("worker-a user:a = %v (answered %v), want [a-1 a-2]", entries, ok)
	}
	if entries, ok := got["worker-a"]["user:b"]; !ok || len(entries) != 0 {
		t.Fatalf("worker-a user:b = %v (answered %v), want an empty answer", entries, ok)
	}
	for _, sessionID := range []string{"user:a", "user:b"} {
		if entries, ok := got["worker-b"][sessionID]; !ok || len(entries) != 0 {
			t.Fatalf("worker-b %s = %v (answered %v), want NotFound read as no entries", sessionID, entries, ok)
		}
	}
}

func TestPoolWhitelistEntryIDsLeavesFailedWorkerUnknown(t *testing.T) {
	healthy := &statusAIServer{entries: map[string][]string{"user:a": {"a-1"}, "user:b": {"b-1"}}}
	down := &statusAIServer{err: status.Error(codes.Unavailable, "worker down")}
	pool := &Pool{clients: []*Client{
		newStatusClient(t, "worker-a", healthy),
		newStatusClient(t, "worker-b", down),
	}}

	got := pool.WhitelistEntryIDs(context.Background(), []string{"user:a", "user:b"})

	if len(got["worker-a"]) != 2 {
		t.Fatalf("worker-a answers = %v, want both sessions", got["worker-a"])
	}
	// 실패한 워커는 결과에 남되 답이 없다. 빈 목록으로 채우면 호출자가 그 워커의 모든
	// 얼굴을 잃은 것으로 판정한다.
	answers, listed := got["worker-b"]
	if !listed || len(answers) != 0 {
		t.Fatalf("worker-b answers = %v (listed %v), want listed with no answers", answers, listed)
	}
	// 첫 실패 뒤로는 묻지 않는다. 멈춘 워커가 세션 수만큼 타임아웃을 쌓으면 안 된다.
	if calls := down.calls.Load(); calls != 1 {
		t.Fatalf("calls to the failing worker = %d, want 1", calls)
	}
}
