package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/ai"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/origin"
	"inno-live-server/internal/session"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// rolloutWorker는 세션별 화이트리스트 임베딩과 preflight 왕복을 흉내 내는 AI 워커다.
type rolloutWorker struct {
	aiv1.UnimplementedAiProcessorServer
	model string

	mu       sync.Mutex
	sessions map[string][]*aiv1.WhitelistEmbedding
}

func (w *rolloutWorker) ProcessVideo(stream grpc.BidiStreamingServer[aiv1.VideoChunk, aiv1.ProcessedVideoChunk]) error {
	for {
		request, err := stream.Recv()
		if err != nil {
			return nil
		}
		if err := stream.Send(&aiv1.ProcessedVideoChunk{Data: []byte("frame"), Timestamp: request.GetTimestamp(), StatusMessage: "success"}); err != nil {
			return err
		}
	}
}

func (w *rolloutWorker) ListSessions(context.Context, *aiv1.ListSessionsRequest) (*aiv1.ListSessionsResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	response := &aiv1.ListSessionsResponse{}
	for sessionID, entries := range w.sessions {
		response.Sessions = append(response.Sessions, &aiv1.SessionInfo{SessionId: sessionID, EntryCount: uint32(len(entries))})
	}
	return response, nil
}

func (w *rolloutWorker) ExportWhitelist(_ context.Context, request *aiv1.ExportWhitelistRequest) (*aiv1.ExportWhitelistResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return &aiv1.ExportWhitelistResponse{SessionId: request.GetSessionId(), EmbeddingModel: w.model, Entries: w.sessions[request.GetSessionId()]}, nil
}

func (w *rolloutWorker) ImportWhitelist(_ context.Context, request *aiv1.ImportWhitelistRequest) (*aiv1.WhitelistResponse, error) {
	if request.GetEmbeddingModel() != w.model {
		return nil, status.Error(codes.FailedPrecondition, "embedding model mismatch")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, entry := range request.GetEntries() {
		if !slices.Contains(w.entryIDsLocked(request.GetSessionId()), entry.GetEntryId()) {
			w.sessions[request.GetSessionId()] = append(w.sessions[request.GetSessionId()], entry)
		}
	}
	return &aiv1.WhitelistResponse{StatusMessage: "success"}, nil
}

func (w *rolloutWorker) GetWhitelistStatus(_ context.Context, request *aiv1.GetWhitelistStatusRequest) (*aiv1.GetWhitelistStatusResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return &aiv1.GetWhitelistStatusResponse{SessionId: request.GetSessionId(), EntryIds: w.entryIDsLocked(request.GetSessionId())}, nil
}

func (w *rolloutWorker) DeleteWhitelist(_ context.Context, request *aiv1.DeleteWhitelistRequest) (*aiv1.WhitelistResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	entries := w.sessions[request.GetSessionId()]
	w.sessions[request.GetSessionId()] = slices.DeleteFunc(entries, func(entry *aiv1.WhitelistEmbedding) bool {
		return entry.GetEntryId() == request.GetEntryId()
	})
	return &aiv1.WhitelistResponse{StatusMessage: "success", EntryId: request.GetEntryId()}, nil
}

func (w *rolloutWorker) entryIDsLocked(sessionID string) []string {
	var ids []string
	for _, entry := range w.sessions[sessionID] {
		ids = append(ids, entry.GetEntryId())
	}
	return ids
}

func (w *rolloutWorker) entryIDs(sessionID string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.entryIDsLocked(sessionID)
}

func startRolloutWorker(t *testing.T, model string, sessions map[string][]string) (*rolloutWorker, string) {
	t.Helper()
	worker := &rolloutWorker{model: model, sessions: make(map[string][]*aiv1.WhitelistEmbedding)}
	for sessionID, entryIDs := range sessions {
		for _, entryID := range entryIDs {
			worker.sessions[sessionID] = append(worker.sessions[sessionID], &aiv1.WhitelistEmbedding{EntryId: entryID, Values: []float32{1, 0}})
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	aiv1.RegisterAiProcessorServer(grpcServer, worker)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	return worker, listener.Addr().String()
}

const testAIControlToken = "test-ai-control-token-0123456789abcdef"

func newAIWorkerTestApplication(t *testing.T, targets ...string) (*Server, *ai.Pool, *httptest.Server) {
	t.Helper()
	pool, err := ai.NewPool(targets, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	manager, err := session.NewManager(config.Config{
		PrivacyMode:    config.PrivacyModeBypass,
		FFmpegPath:     "ffmpeg",
		UDPPortMin:     42200,
		UDPPortMax:     42300,
		FrameQueueSize: 2,
		MaxSessions:    2,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	origins, err := origin.NewConfig(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	application := New(config.Config{
		ReferenceStorePath: filepath.Join(t.TempDir(), "reference-faces.json"),
		AIPreflightTimeout: 5 * time.Second,
		AIWireFormat:       config.WireFormatJPEG,
		AIControlToken:     testAIControlToken,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), manager, pool, origins, nil, nil)
	control := httptest.NewServer(application.AIControlHandler())
	t.Cleanup(control.Close)
	return application, pool, control
}

func postReplace(t *testing.T, control *httptest.Server, token, from, to string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"from": from, "to": to})
	request, _ := http.NewRequest(http.MethodPost, control.URL+"/ai/workers/replace", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(response.Body).Decode(&decoded)
	return response.StatusCode, decoded
}

// 옛 워커를 새 워커로 바꾸면 등록 얼굴이 같은 엔트리 id로 옮겨 가고, 기준 얼굴
// 기록도 새 워커를 가리키므로 재등록이 필요 없다(#515).
func TestReplaceAIWorkerMovesWhitelistsAndRemovesOldWorker(t *testing.T) {
	_, oldAddr := startRolloutWorker(t, "ir18:a", map[string][]string{"client-1": {"e1", "e2"}, "default": {"env"}})
	fresh, newAddr := startRolloutWorker(t, "ir18:a", nil)
	application, pool, control := newAIWorkerTestApplication(t, oldAddr)
	application.references.faces["client-1"] = []referenceFace{
		{FaceID: "f1", EntryIDs: map[string]string{oldAddr: "e1"}},
		{FaceID: "f2", EntryIDs: map[string]string{oldAddr: "e2"}},
	}

	code, body := postReplace(t, control, testAIControlToken, oldAddr, newAddr)
	if code != http.StatusOK || body["removed"] != true || body["copied_sessions"] != float64(2) {
		t.Fatalf("replace = %d %v, want 200 with old worker removed and 2 sessions copied", code, body)
	}
	if got := fresh.entryIDs("client-1"); !slices.Equal(got, []string{"e1", "e2"}) {
		t.Fatalf("new worker client-1 entries = %v", got)
	}
	if got := fresh.entryIDs("default"); !slices.Equal(got, []string{"env"}) {
		t.Fatalf("new worker kept no env reference face: %v", got)
	}
	targets := pool.Targets()
	if len(targets) != 1 || targets[0].Address != newAddr || targets[0].Draining {
		t.Fatalf("pool targets = %+v, want only the open new worker", targets)
	}
	for _, face := range application.references.snapshot()["client-1"] {
		if face.EntryIDs[newAddr] == "" || face.EntryIDs[oldAddr] != "" {
			t.Fatalf("reference record %s entry ids = %v, want only the new worker", face.FaceID, face.EntryIDs)
		}
		verdict := judgeReferenceFace(face, "client-1", map[string]map[string][]string{newAddr: {"client-1": fresh.entryIDs("client-1")}})
		if !verdict.complete || len(verdict.missing) > 0 {
			t.Fatalf("reference check verdict for %s = %+v, want present on every worker", face.FaceID, verdict)
		}
	}
}

// 복사하는 사이 지워진 얼굴이 새 워커에 남으면 그 사람이 블러되지 않으므로 지운다.
func TestReplaceAIWorkerPrunesEntriesDeletedDuringCopy(t *testing.T) {
	_, oldAddr := startRolloutWorker(t, "ir18:a", map[string][]string{"client-1": {"e1"}})
	fresh, newAddr := startRolloutWorker(t, "ir18:a", map[string][]string{"client-1": {"deleted"}})
	application, _, _ := newAIWorkerTestApplication(t, oldAddr)
	application.references.faces["client-1"] = []referenceFace{{FaceID: "f1", EntryIDs: map[string]string{oldAddr: "e1"}}}

	result, err := application.replaceAIWorker(context.Background(), oldAddr, newAddr)
	if err != nil {
		t.Fatalf("replaceAIWorker() error = %v", err)
	}
	if result.PrunedEntries != 1 {
		t.Fatalf("pruned = %d, want 1", result.PrunedEntries)
	}
	if got := fresh.entryIDs("client-1"); !slices.Equal(got, []string{"e1"}) {
		t.Fatalf("new worker entries = %v, want only e1", got)
	}
}

// 인식 모델이 다르면 옮기지 않고 처음 상태로 돌린다. 옛 워커는 그대로 세션을 받는다.
func TestReplaceAIWorkerRollsBackOnEmbeddingModelMismatch(t *testing.T) {
	_, oldAddr := startRolloutWorker(t, "ir18:a", map[string][]string{"client-1": {"e1"}})
	_, newAddr := startRolloutWorker(t, "ir18:b", nil)
	application, pool, control := newAIWorkerTestApplication(t, oldAddr)
	application.references.faces["client-1"] = []referenceFace{{FaceID: "f1", EntryIDs: map[string]string{oldAddr: "e1"}}}

	code, body := postReplace(t, control, testAIControlToken, oldAddr, newAddr)
	if code != http.StatusConflict {
		t.Fatalf("replace = %d %v, want 409", code, body)
	}
	targets := pool.Targets()
	if len(targets) != 1 || targets[0].Address != oldAddr || targets[0].Draining {
		t.Fatalf("pool targets = %+v, want only the old worker still open", targets)
	}
	if ids := application.references.snapshot()["client-1"][0].EntryIDs; ids[newAddr] != "" {
		t.Fatalf("reference record kept the aborted worker: %v", ids)
	}
}

// 남은 세션이 있으면 옛 워커를 빼지 않고 배정 제외로만 둔다. 다시 요청하면 이어서 한다.
func TestReplaceAIWorkerKeepsOldWorkerWhileSessionsRemain(t *testing.T) {
	_, oldAddr := startRolloutWorker(t, "ir18:a", nil)
	_, newAddr := startRolloutWorker(t, "ir18:a", nil)
	_, pool, control := newAIWorkerTestApplication(t, oldAddr)
	_, release := pool.Acquire(false)

	code, body := postReplace(t, control, testAIControlToken, oldAddr, newAddr)
	if code != http.StatusAccepted || body["removed"] != false {
		t.Fatalf("replace = %d %v, want 202 with the old worker kept", code, body)
	}
	for _, target := range pool.Targets() {
		if target.Address == oldAddr && !target.Draining {
			t.Fatalf("old worker = %+v, want draining", target)
		}
	}

	release()
	code, body = postReplace(t, control, testAIControlToken, oldAddr, newAddr)
	if code != http.StatusOK || body["removed"] != true {
		t.Fatalf("retried replace = %d %v, want 200 with the old worker removed", code, body)
	}
}

func TestAIControlRequiresToken(t *testing.T) {
	_, oldAddr := startRolloutWorker(t, "ir18:a", nil)
	_, _, control := newAIWorkerTestApplication(t, oldAddr)

	for _, header := range []string{"", "Bearer wrong", "Basic " + testAIControlToken, testAIControlToken} {
		request, _ := http.NewRequest(http.MethodGet, control.URL+"/ai/workers", nil)
		if header != "" {
			request.Header.Set("Authorization", header)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("Authorization %q = %d, want 401", header, response.StatusCode)
		}
	}

	request, _ := http.NewRequest(http.MethodGet, control.URL+"/ai/workers", nil)
	request.Header.Set("Authorization", "Bearer "+testAIControlToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(payload), oldAddr) {
		t.Fatalf("GET /ai/workers = %d %s", response.StatusCode, payload)
	}
}
