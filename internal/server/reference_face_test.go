package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
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

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeAIWorker는 실제 Python 워커의 화이트리스트 계약을 재현한다. 워커마다 자기
// 엔트리 id를 발급하고, 빈 엔트리 id는 바로 거절하며, 발급한 적 없는 id를 지우면
// NOT_FOUND다.
type fakeAIWorker struct {
	aiv1.UnimplementedAiProcessorServer
	name string

	mu      sync.Mutex
	entries []string
	next    int

	addStarted       chan struct{}
	allowAdd         chan struct{}
	addStartedOnce   sync.Once
	clearStarted     chan struct{}
	allowClear       chan struct{}
	clearStartedOnce sync.Once

	// onAdd가 있으면 AddWhitelist마다 시작할 때 이미 등록된 엔트리 수와 호출
	// 컨텍스트로 부른다. 같은 업로드의 두 파일 사이에 일어나는 일을 테스트가 조종한다.
	onAdd func(ctx context.Context, registered int)
}

func (w *fakeAIWorker) AddWhitelist(ctx context.Context, request *aiv1.FaceData) (*aiv1.WhitelistResponse, error) {
	if request.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id must not be empty")
	}
	if w.addStarted != nil {
		w.addStartedOnce.Do(func() { close(w.addStarted) })
		select {
		case <-w.allowAdd:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if bytes.Contains(request.GetData(), []byte("REJECT")) {
		return &aiv1.WhitelistResponse{StatusMessage: "failed: No face detected"}, nil
	}
	if w.onAdd != nil {
		w.mu.Lock()
		registered := len(w.entries)
		w.mu.Unlock()
		w.onAdd(ctx, registered)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.next++
	entryID := fmt.Sprintf("%s-%d", w.name, w.next)
	w.entries = append(w.entries, entryID)
	return &aiv1.WhitelistResponse{StatusMessage: "success", EntryId: entryID, EntryCount: uint32(len(w.entries))}, nil
}

func (w *fakeAIWorker) DeleteWhitelist(_ context.Context, request *aiv1.DeleteWhitelistRequest) (*aiv1.WhitelistResponse, error) {
	if strings.TrimSpace(request.GetEntryId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "entry_id must not be empty or whitespace-only")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	remaining := make([]string, 0, len(w.entries))
	for _, entry := range w.entries {
		if entry != request.GetEntryId() {
			remaining = append(remaining, entry)
		}
	}
	if len(remaining) == len(w.entries) {
		return nil, status.Errorf(codes.NotFound, "whitelist entry %q does not exist", request.GetEntryId())
	}
	w.entries = remaining
	return &aiv1.WhitelistResponse{StatusMessage: "success", EntryId: request.GetEntryId()}, nil
}

func (w *fakeAIWorker) GetWhitelistStatus(ctx context.Context, _ *aiv1.GetWhitelistStatusRequest) (*aiv1.GetWhitelistStatusResponse, error) {
	if w.clearStarted != nil {
		w.clearStartedOnce.Do(func() { close(w.clearStarted) })
		select {
		case <-w.allowClear:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &aiv1.GetWhitelistStatusResponse{EntryIds: w.snapshot()}, nil
}

func (w *fakeAIWorker) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.entries...)
}

// newReferenceFaceTestServer는 AI 워커 workerCount개와, 그 전부를 담은 풀에 연결한
// HTTP 서버를 띄운다.
func newReferenceFaceTestServer(t *testing.T, workerCount int) (*httptest.Server, []*fakeAIWorker, string) {
	t.Helper()
	workers := make([]*fakeAIWorker, 0, workerCount)
	targets := make([]string, 0, workerCount)
	for index := range workerCount {
		worker := &fakeAIWorker{name: fmt.Sprintf("worker%d", index)}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		grpcServer := grpc.NewServer()
		aiv1.RegisterAiProcessorServer(grpcServer, worker)
		go grpcServer.Serve(listener)
		t.Cleanup(grpcServer.Stop)
		workers = append(workers, worker)
		targets = append(targets, listener.Addr().String())
	}

	pool, err := ai.NewPool(targets, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	storePath := filepath.Join(t.TempDir(), "reference-faces.json")
	origins, err := origin.NewConfig(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	application := New(
		config.Config{ReferenceStorePath: storePath},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics.New(),
		nil,
		pool,
		origins,
		nil,
		nil,
	)
	httpServer := httptest.NewServer(application.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer, workers, storePath
}

func uploadReferenceFaces(t *testing.T, baseURL string, count int) referenceStatus {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for index := range count {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="images"; filename="face-%d.jpg"`, index))
		header.Set("Content-Type", "image/jpeg")
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(fmt.Sprintf("face-%d", index))); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	response, err := http.Post(baseURL+"/reference-face", writer.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("POST /reference-face status = %d, body = %s", response.StatusCode, data)
	}
	var registered referenceStatus
	if err := json.NewDecoder(response.Body).Decode(&registered); err != nil {
		t.Fatal(err)
	}
	if registered.Count != count {
		t.Fatalf("registered count = %d, want %d", registered.Count, count)
	}
	return registered
}

func deleteReferenceFace(t *testing.T, url string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestGuestReferenceUploadDoesNotRepopulateAfterSessionDelete(t *testing.T) {
	worker := &fakeAIWorker{name: "worker", addStarted: make(chan struct{}), allowAdd: make(chan struct{})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	aiv1.RegisterAiProcessorServer(grpcServer, worker)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)

	pool, err := ai.NewPool([]string{listener.Addr().String()}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	manager, err := session.NewManager(config.Config{
		PrivacyMode:    config.PrivacyModeBypass,
		FFmpegPath:     "ffmpeg",
		UDPPortMin:     42000,
		UDPPortMax:     42100,
		FrameQueueSize: 2,
		MaxSessions:    2,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	redisServer, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(redisServer.Close)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	origins, err := origin.NewConfig(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	application := New(config.Config{ReferenceStorePath: filepath.Join(t.TempDir(), "reference-faces.json")}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), manager, pool, origins, nil, nil)
	application.SetGuestQueue(&GuestQueue{client: client, sessions: manager, ttl: time.Minute, admissionTTL: time.Minute, maxGuests: 1})
	httpServer := httptest.NewServer(application.Handler())
	t.Cleanup(httpServer.Close)

	guest := "guest-cookie"
	live, owner, err := manager.CreateForGuest(guestHash(guest), nil)
	if err != nil {
		t.Fatal(err)
	}
	uploadRequest := guestReferenceUploadRequest(t, httpServer.URL, live.ID, owner, guest)
	uploadResult := make(chan *http.Response, 1)
	uploadError := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(uploadRequest)
		if err != nil {
			uploadError <- err
			return
		}
		uploadResult <- response
	}()
	select {
	case <-worker.addStarted:
	case <-time.After(time.Second):
		t.Fatal("upload did not reach AI worker")
	}

	deleteRequest, err := http.NewRequest(http.MethodDelete, httpServer.URL+"/guest/sessions/"+live.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	deleteRequest.AddCookie(&http.Cookie{Name: guestCookieName, Value: guest})
	deleteRequest.Header.Set("X-Session-Owner-Token", owner)
	deleteResponse, err := http.DefaultClient.Do(deleteRequest)
	if err != nil {
		t.Fatal(err)
	}
	if deleteResponse.StatusCode != http.StatusNoContent {
		data, _ := io.ReadAll(deleteResponse.Body)
		deleteResponse.Body.Close()
		t.Fatalf("DELETE guest session = %d: %s", deleteResponse.StatusCode, data)
	}
	deleteResponse.Body.Close()
	close(worker.allowAdd)

	select {
	case err := <-uploadError:
		t.Fatal(err)
	case response := <-uploadResult:
		defer response.Body.Close()
		if response.StatusCode == http.StatusCreated {
			t.Fatal("upload completed with 201 after its guest session was deleted")
		}
	case <-time.After(time.Second):
		t.Fatal("upload did not complete")
	}
	deadline := time.Now().Add(time.Second)
	for len(worker.snapshot()) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if entries := worker.snapshot(); len(entries) != 0 {
		t.Fatalf("worker retains entries after guest session deletion: %v", entries)
	}
	if status := application.references.status(live.AIClientID); status.Count != 0 {
		t.Fatalf("reference store count = %d, want 0", status.Count)
	}
}

func TestGuestFaceCleanupCompletesBeforeServerShutdownContinues(t *testing.T) {
	worker := &fakeAIWorker{name: "worker", clearStarted: make(chan struct{}), allowClear: make(chan struct{})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	aiv1.RegisterAiProcessorServer(grpcServer, worker)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	pool, err := ai.NewPool([]string{listener.Addr().String()}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	manager, err := session.NewManager(config.Config{PrivacyMode: config.PrivacyModeBypass, FFmpegPath: "ffmpeg", UDPPortMin: 42000, UDPPortMax: 42100, FrameQueueSize: 2, MaxSessions: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	redisServer, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(redisServer.Close)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	origins, err := origin.NewConfig(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	application := New(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), manager, pool, origins, nil, nil)
	application.SetGuestQueue(&GuestQueue{client: client, sessions: manager, ttl: time.Minute, admissionTTL: time.Minute, maxGuests: 1})
	if _, _, err := manager.CreateForGuest(guestHash("guest"), nil); err != nil {
		t.Fatal(err)
	}
	manager.CloseAll()
	select {
	case <-worker.clearStarted:
	case <-time.After(time.Second):
		t.Fatal("guest whitelist cleanup did not start")
	}
	finished := make(chan error, 1)
	go func() { finished <- application.WaitGuestCleanup(context.Background()) }()
	select {
	case err := <-finished:
		t.Fatalf("cleanup wait returned before worker cleanup finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(worker.allowClear)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup wait did not finish")
	}
}

func TestGuestQueueSSEReportsExpiredTicketWithoutQueueMutation(t *testing.T) {
	redisServer, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(redisServer.Close)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	manager, err := session.NewManager(config.Config{PrivacyMode: config.PrivacyModeBypass, FFmpegPath: "ffmpeg", UDPPortMin: 42000, UDPPortMax: 42100, FrameQueueSize: 2, MaxSessions: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	queue := &GuestQueue{client: client, sessions: manager, ttl: time.Second, admissionTTL: time.Minute, maxGuests: 0}
	guest := "guest-cookie"
	ticket, err := queue.CreateOrGet(context.Background(), guest, "203.0.113.1:443", "")
	if err != nil {
		t.Fatal(err)
	}
	origins, err := origin.NewConfig(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	application := New(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), manager, nil, origins, nil, nil)
	application.SetGuestQueue(queue)
	httpServer := httptest.NewServer(application.Handler())
	t.Cleanup(httpServer.Close)
	previousInterval := guestSSEStatusInterval
	guestSSEStatusInterval = 5 * time.Millisecond
	t.Cleanup(func() { guestSSEStatusInterval = previousInterval })

	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/guest-queue/"+ticket.ID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: guestCookieName, Value: guest})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "event: queue\n" {
		t.Fatalf("initial SSE event = %q, %v", line, err)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	redisServer.FastForward(2 * time.Second)
	if line, err := reader.ReadString('\n'); err != nil || line != "event: expired\n" {
		t.Fatalf("expiry SSE event = %q, %v", line, err)
	}
}

func guestReferenceUploadRequest(t *testing.T, baseURL, sessionID, owner, guest string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="image"; filename="face.jpg"`)
	header.Set("Content-Type", "image/jpeg")
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("face")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/guest/sessions/"+sessionID+"/reference-face", &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Session-Owner-Token", owner)
	request.AddCookie(&http.Cookie{Name: guestCookieName, Value: guest})
	return request
}

// 워커마다 같은 얼굴에 자기 엔트리 id를 발급하므로, 얼굴 하나를 지우려면 각 워커에
// 그 워커의 id로 요청해야 한다. 한 워커의 id를 모두에게 보내면 나머지 워커가
// NOT_FOUND로 답해 요청 전체가 실패하고 얼굴이 등록된 채 남았다.
func TestDeleteReferenceFaceByIDRemovesItFromEveryWorker(t *testing.T) {
	httpServer, workers, _ := newReferenceFaceTestServer(t, 2)
	registered := uploadReferenceFaces(t, httpServer.URL, 2)

	response := deleteReferenceFace(t, httpServer.URL+"/reference-face/"+registered.Faces[0].FaceID)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("DELETE /reference-face/{id} status = %d, body = %s", response.StatusCode, data)
	}

	for index, worker := range workers {
		entries := worker.snapshot()
		want := fmt.Sprintf("worker%d-2", index)
		if len(entries) != 1 || entries[0] != want {
			t.Fatalf("worker %d entries = %v, want only %q", index, entries, want)
		}
	}

	status := getReferenceStatus(t, httpServer.URL)
	if status.Count != 1 || len(status.Faces) != 1 || status.Faces[0].FaceID != registered.Faces[1].FaceID {
		t.Fatalf("status after delete = %+v, want only the second face", status)
	}
}

// 워커는 빈 엔트리 id를 거절하므로 "전부 지우기"는 각 워커가 실제로 가진 id를
// 하나씩 지워야 한다.
func TestDeleteAllReferenceFacesEmptiesEveryWorker(t *testing.T) {
	httpServer, workers, _ := newReferenceFaceTestServer(t, 2)
	uploadReferenceFaces(t, httpServer.URL, 2)

	response := deleteReferenceFace(t, httpServer.URL+"/reference-face")
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("DELETE /reference-face status = %d, body = %s", response.StatusCode, data)
	}

	for index, worker := range workers {
		if entries := worker.snapshot(); len(entries) != 0 {
			t.Fatalf("worker %d entries = %v, want empty", index, entries)
		}
	}
	if status := getReferenceStatus(t, httpServer.URL); status.Registered || status.Count != 0 {
		t.Fatalf("status after delete-all = %+v, want unregistered", status)
	}
}

// 이미지 한 장 업로드는 클라이언트의 얼굴 집합을 교체한다. 이전 얼굴이 실제로
// 워커에서 빠져야만 참이다.
func TestReplaceUploadClearsPreviousWorkerEntries(t *testing.T) {
	httpServer, workers, _ := newReferenceFaceTestServer(t, 2)
	uploadReferenceFaces(t, httpServer.URL, 2)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="image"; filename="replacement.jpg"`)
	header.Set("Content-Type", "image/jpeg")
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(httpServer.URL+"/reference-face", writer.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("replace upload status = %d, body = %s", response.StatusCode, data)
	}

	for index, worker := range workers {
		entries := worker.snapshot()
		want := fmt.Sprintf("worker%d-3", index)
		if len(entries) != 1 || entries[0] != want {
			t.Fatalf("worker %d entries = %v, want only the replacement %q", index, entries, want)
		}
	}
}

// 엔트리 id가 있어야 얼굴을 지울 수 있으므로, 재시작 뒤에도 남되 API 응답에는
// 나오지 않아야 한다.
func TestReferenceStorePersistsWorkerEntryIDsWithoutExposingThem(t *testing.T) {
	httpServer, _, storePath := newReferenceFaceTestServer(t, 2)
	registered := uploadReferenceFaces(t, httpServer.URL, 1)

	response, err := http.Get(httpServer.URL + "/reference-face")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if strings.Contains(string(payload), "entry_id") {
		t.Fatalf("API response leaks worker entry ids: %s", payload)
	}

	reloaded := newReferenceStore(storePath, false)
	faces := reloaded.faces["default"]
	if len(faces) != 1 || faces[0].FaceID != registered.Faces[0].FaceID {
		t.Fatalf("reloaded faces = %+v, want the registered face", faces)
	}
	if len(faces[0].EntryIDs) != 2 {
		t.Fatalf("reloaded entry ids = %v, want one per worker", faces[0].EntryIDs)
	}
}

func TestReferenceStoreDeleteClientRestoresDataWhenSaveFails(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "reference-faces.json")
	store := newReferenceStore(path, false)
	store.faces = map[string][]referenceFace{
		"target": {{FaceID: "target-face"}},
		"other":  {{FaceID: "other-face"}},
	}
	if err := store.save(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 저장 경로를 일반 파일 아래로 둔다. MkdirAll이 쓰기 전에 실패하고, 이전에 공개한
	// 메타데이터 파일은 보존 확인을 위해 그대로 남는다.
	store.path = filepath.Join(path, "cannot-create-child")
	if err := store.deleteClient("target"); err == nil {
		t.Fatal("deleteClient unexpectedly succeeded when saving was unavailable")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("original metadata after failed save = (%q, %v), want unchanged file", got, err)
	}
	if faces := store.faces["target"]; len(faces) != 1 || faces[0].FaceID != "target-face" {
		t.Fatalf("target faces after failed save = %+v, want restored data", faces)
	}
	if faces := store.faces["other"]; len(faces) != 1 || faces[0].FaceID != "other-face" {
		t.Fatalf("other user's faces after failed save = %+v, want preserved data", faces)
	}

	store.path = path
	if err := store.deleteClient("target"); err != nil {
		t.Fatalf("retry deleteClient failed: %v", err)
	}
	reloaded := newReferenceStore(path, false)
	if _, ok := reloaded.faces["target"]; ok {
		t.Fatal("target faces remained after successful retry")
	}
	if faces := reloaded.faces["other"]; len(faces) != 1 || faces[0].FaceID != "other-face" {
		t.Fatalf("other user's faces after retry = %+v, want preserved data", faces)
	}
}

// referenceUploadPart는 postReferenceFaces에 넘길 multipart 파일 하나다.
type referenceUploadPart struct {
	field       string
	filename    string
	contentType string
	content     string
}

// postReferenceFaces는 임의의 파트를 올리고 응답을 그대로 돌려준다.
func postReferenceFaces(t *testing.T, baseURL string, parts []referenceUploadPart) *http.Response {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, p := range parts {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, p.field, p.filename))
		header.Set("Content-Type", p.contentType)
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(p.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(baseURL+"/reference-face", writer.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// 뒤 파일이 형식 검사에 실패해도, 같은 요청에서 이미 워커에 등록한 앞 파일이 블러
// 제외로 남으면 안 된다.
func TestPartialUploadFormatFailureLeavesNoWorkerEntries(t *testing.T) {
	httpServer, workers, _ := newReferenceFaceTestServer(t, 2)

	response := postReferenceFaces(t, httpServer.URL, []referenceUploadPart{
		{field: "images", filename: "ok.jpg", contentType: "image/jpeg", content: "ok"},
		{field: "images", filename: "bad.txt", contentType: "text/plain", content: "nope"},
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 400; body = %s", response.StatusCode, data)
	}

	for index, worker := range workers {
		if entries := worker.snapshot(); len(entries) != 0 {
			t.Fatalf("worker %d entries = %v, want empty after format failure", index, entries)
		}
	}
	if status := getReferenceStatus(t, httpServer.URL); status.Count != 0 {
		t.Fatalf("status after failure = %+v, want count 0", status)
	}
}

// 뒤 파일을 워커가 거절하면 이 요청의 앞선 등록은 되돌리고, 이전 요청의 얼굴은
// 그대로 둔다.
func TestPartialUploadWorkerRejectionRollsBackThisRequestOnly(t *testing.T) {
	httpServer, workers, _ := newReferenceFaceTestServer(t, 2)
	uploadReferenceFaces(t, httpServer.URL, 1) // 이전 요청이 등록한 얼굴

	response := postReferenceFaces(t, httpServer.URL, []referenceUploadPart{
		{field: "images", filename: "ok.jpg", contentType: "image/jpeg", content: "ok"},
		{field: "images", filename: "reject.jpg", contentType: "image/jpeg", content: "REJECT"},
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 400; body = %s", response.StatusCode, data)
	}

	for index, worker := range workers {
		entries := worker.snapshot()
		want := fmt.Sprintf("worker%d-1", index)
		if len(entries) != 1 || entries[0] != want {
			t.Fatalf("worker %d entries = %v, want only the pre-existing %q", index, entries, want)
		}
	}
	if status := getReferenceStatus(t, httpServer.URL); status.Count != 1 {
		t.Fatalf("status after rejection = %+v, want count 1 (pre-existing face kept)", status)
	}
}

func getReferenceStatus(t *testing.T, baseURL string) referenceStatus {
	t.Helper()
	response, err := http.Get(baseURL + "/reference-face")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var status referenceStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// 업로드 도중 클라이언트가 끊으면 요청 컨텍스트가 취소되고, 그것이 뒤 AddWhitelist가
// 실패하는 경로 중 하나다. 그래도 롤백은 앞 파일이 등록한 엔트리를 지워야 한다
// (#201) — 취소된 컨텍스트로 돌리면 워커가 그 얼굴을 계속 블러에서 제외한다.
func TestUploadCancelledMidRequestRollsBackRegisteredEntries(t *testing.T) {
	httpServer, workers, _ := newReferenceFaceTestServer(t, 1)
	worker := workers[0]

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	// 첫 파일이 등록되고 두 번째 파일의 호출이 시작되면 클라이언트를 끊는다. 서버가
	// 취소를 관측할 때까지 기다려 두 번째 호출의 실패를 결정적으로 만든다.
	worker.onAdd = func(ctx context.Context, registered int) {
		if registered != 1 {
			return
		}
		cancelRequest()
		<-ctx.Done()
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, name := range []string{"first.jpg", "second.jpg"} {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="images"; filename=%q`, name))
		header.Set("Content-Type", "image/jpeg")
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte("ok")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, httpServer.URL+"/reference-face", &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	if response, err := http.DefaultClient.Do(request); err == nil {
		response.Body.Close()
		t.Fatal("request succeeded, want client cancellation")
	}

	// 핸들러는 취소된 클라이언트보다 오래 살므로 롤백을 기다린다.
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries := worker.snapshot()
		if len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker entries = %v, want empty after rollback", entries)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status := getReferenceStatus(t, httpServer.URL); status.Count != 0 {
		t.Fatalf("status after cancelled upload = %+v, want count 0", status)
	}
}

// 끝내 다 도착하지 않는 본문은 핸들러와 연결을 붙잡지 말고 업로드 읽기 기한에
// 걸려야 한다(#207). 클라이언트가 언제 쓰기를 멈추는지가 아니라 서버를 검증하도록
// 원시 연결로 보낸다.
func TestUploadStalledBodyHitsReadDeadline(t *testing.T) {
	previous := referenceUploadReadTimeout
	referenceUploadReadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { referenceUploadReadTimeout = previous })

	httpServer, _, _ := newReferenceFaceTestServer(t, 1)

	var head bytes.Buffer
	writer := multipart.NewWriter(&head)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="images"; filename="slow.jpg"`)
	header.Set("Content-Type", "image/jpeg")
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	// writer는 일부러 열어 둔다. multipart 종결자가 오지 않으므로 핸들러는 기한이
	// 될 때까지 읽는다.

	address := strings.TrimPrefix(httpServer.URL, "http://")
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	// Content-Length는 크게 주고 첫 조각만 보내는 것이 찔끔 보내는 클라이언트다.
	// 서버는 나머지를 영원히 기다리면 안 된다.
	request := fmt.Sprintf("POST /reference-face HTTP/1.1\r\nHost: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n",
		address, writer.FormDataContentType(), head.Len()+1<<20)
	if _, err := connection.Write(append([]byte(request), head.Bytes()...)); err != nil {
		t.Fatal(err)
	}

	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatalf("stalled upload was not cut off by the read deadline: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a body that never finished arriving", response.StatusCode)
	}
}
