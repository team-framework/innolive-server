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

// fakeAIWorker reproduces the whitelist contract of the real Python worker:
// each worker mints its own entry id, an empty entry id is rejected outright,
// and deleting an id the worker never issued is NOT_FOUND.
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

// newReferenceFaceTestServer starts workerCount AI workers and an HTTP server
// wired to a pool covering all of them.
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

// Every worker mints its own entry id for the same face, so deleting one face
// has to address each worker with that worker's id. Sending one worker's id to
// all of them made the other workers answer NOT_FOUND, failing the whole
// request and leaving the face registered.
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

// The workers reject an empty entry id, so "delete everything" must enumerate
// the ids each worker actually holds.
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

// A single-image upload replaces the client's set, which is only true if the
// previous faces actually leave the workers.
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

// Entry ids are what makes a face deletable, so they have to survive a restart
// while staying out of API responses.
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
