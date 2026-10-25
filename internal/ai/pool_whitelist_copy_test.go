package ai

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// embeddingAIServer는 세션별 임베딩을 메모리에 두는 워커 흉내다.
type embeddingAIServer struct {
	aiv1.UnimplementedAiProcessorServer
	model string

	mu       sync.Mutex
	sessions map[string][]*aiv1.WhitelistEmbedding
}

func newEmbeddingAIServer(model string, sessions map[string][]string) *embeddingAIServer {
	server := &embeddingAIServer{model: model, sessions: make(map[string][]*aiv1.WhitelistEmbedding)}
	for sessionID, entryIDs := range sessions {
		for _, entryID := range entryIDs {
			server.sessions[sessionID] = append(server.sessions[sessionID], &aiv1.WhitelistEmbedding{EntryId: entryID, Values: []float32{1, 0}})
		}
	}
	return server
}

func (s *embeddingAIServer) ListSessions(context.Context, *aiv1.ListSessionsRequest) (*aiv1.ListSessionsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	response := &aiv1.ListSessionsResponse{}
	for sessionID, entries := range s.sessions {
		response.Sessions = append(response.Sessions, &aiv1.SessionInfo{SessionId: sessionID, EntryCount: uint32(len(entries))})
	}
	return response, nil
}

func (s *embeddingAIServer) ExportWhitelist(_ context.Context, request *aiv1.ExportWhitelistRequest) (*aiv1.ExportWhitelistResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &aiv1.ExportWhitelistResponse{SessionId: request.GetSessionId(), EmbeddingModel: s.model, Entries: s.sessions[request.GetSessionId()]}, nil
}

func (s *embeddingAIServer) ImportWhitelist(_ context.Context, request *aiv1.ImportWhitelistRequest) (*aiv1.WhitelistResponse, error) {
	if request.GetEmbeddingModel() != s.model {
		return nil, status.Error(codes.FailedPrecondition, "embedding model mismatch")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range request.GetEntries() {
		if !slices.ContainsFunc(s.sessions[request.GetSessionId()], func(e *aiv1.WhitelistEmbedding) bool { return e.GetEntryId() == entry.GetEntryId() }) {
			s.sessions[request.GetSessionId()] = append(s.sessions[request.GetSessionId()], entry)
		}
	}
	return &aiv1.WhitelistResponse{StatusMessage: "success", EntryCount: uint32(len(s.sessions[request.GetSessionId()]))}, nil
}

func (s *embeddingAIServer) entryIDs(sessionID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, entry := range s.sessions[sessionID] {
		ids = append(ids, entry.GetEntryId())
	}
	return ids
}

func newEmbeddingClient(t *testing.T, address string, server *embeddingAIServer) *Client {
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

// 새 워커는 옛 워커의 등록 얼굴을 같은 엔트리 id로 받는다(#515).
func TestPoolCopyWhitelistsKeepsEntryIDs(t *testing.T) {
	old := newEmbeddingAIServer("ir18:a", map[string][]string{"client-1": {"e1", "e2"}, "client-2": {"e3"}, "idle": nil})
	fresh := newEmbeddingAIServer("ir18:a", map[string][]string{"client-2": {"e3"}})
	pool := &Pool{clients: []*Client{newEmbeddingClient(t, "old", old), newEmbeddingClient(t, "new", fresh)}, active: make([]int, 2)}

	copied, err := pool.CopyWhitelists(context.Background(), "old", "new")
	if err != nil {
		t.Fatalf("CopyWhitelists() error = %v", err)
	}
	if got := fresh.entryIDs("client-1"); !slices.Equal(got, []string{"e1", "e2"}) {
		t.Fatalf("new worker client-1 entries = %v, want e1 e2", got)
	}
	if got := fresh.entryIDs("client-2"); !slices.Equal(got, []string{"e3"}) {
		t.Fatalf("new worker client-2 entries = %v, want e3 once", got)
	}
	if len(copied) != 2 || !slices.Equal(copied["client-1"], []string{"e1", "e2"}) {
		t.Fatalf("copied = %v", copied)
	}
}

// 인식 모델이 다른 워커 사이에서는 옮기지 않고 그 사실을 알린다.
func TestPoolCopyWhitelistsReportsModelMismatch(t *testing.T) {
	old := newEmbeddingAIServer("ir18:a", map[string][]string{"client-1": {"e1"}})
	fresh := newEmbeddingAIServer("ir18:b", nil)
	pool := &Pool{clients: []*Client{newEmbeddingClient(t, "old", old), newEmbeddingClient(t, "new", fresh)}, active: make([]int, 2)}

	_, err := pool.CopyWhitelists(context.Background(), "old", "new")
	if !errors.Is(err, ErrEmbeddingModelMismatch) {
		t.Fatalf("CopyWhitelists() error = %v, want ErrEmbeddingModelMismatch", err)
	}
	if got := fresh.entryIDs("client-1"); len(got) != 0 {
		t.Fatalf("new worker entries = %v, want none", got)
	}
	if _, err := pool.CopyWhitelists(context.Background(), "old", "missing"); err == nil {
		t.Fatal("CopyWhitelists() to an unknown target succeeded")
	}
}
