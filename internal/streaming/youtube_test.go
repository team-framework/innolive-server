package streaming

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"inno-live-server/internal/auth"

	"github.com/google/uuid"
)

type stubTokens struct{ token string }

func (s stubTokens) AccessToken(context.Context, uuid.UUID) (string, error) {
	return s.token, nil
}

type memoryStore struct {
	mu       sync.Mutex
	accounts map[uuid.UUID]auth.StreamingAccount
}

func newMemoryStore() *memoryStore {
	return &memoryStore{accounts: make(map[uuid.UUID]auth.StreamingAccount)}
}

func (s *memoryStore) Upsert(_ context.Context, account auth.StreamingAccount) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if account.ID == uuid.Nil {
		account.ID = uuid.New()
	}
	s.accounts[account.ID] = account
	return nil
}

func (s *memoryStore) UpdateChannel(_ context.Context, id uuid.UUID, channelID string, channelTitle *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.accounts[id]
	if !ok {
		return auth.ErrStreamingAccountNotFound
	}
	account.ChannelID = channelID
	account.ChannelTitle = channelTitle
	s.accounts[id] = account
	return nil
}

func (s *memoryStore) Delete(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[id]; !ok {
		return auth.ErrStreamingAccountNotFound
	}
	delete(s.accounts, id)
	return nil
}

func (s *memoryStore) ListByUser(_ context.Context, userID uuid.UUID) ([]auth.StreamingAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var accounts []auth.StreamingAccount
	for _, account := range s.accounts {
		if account.UserID == userID {
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}

func (s *memoryStore) Get(_ context.Context, userID uuid.UUID, provider auth.StreamingProvider) (auth.StreamingAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, account := range s.accounts {
		if account.UserID == userID && account.Provider == provider {
			return account, nil
		}
	}
	return auth.StreamingAccount{}, auth.ErrStreamingAccountNotFound
}

func (s *memoryStore) UpdateRefreshToken(_ context.Context, id uuid.UUID, ciphertext []byte, version *int16, expiresAt *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.accounts[id]
	if !ok {
		return auth.ErrStreamingAccountNotFound
	}
	account.RefreshTokenCiphertext = ciphertext
	account.TokenKeyVersion = version
	account.RefreshTokenExpiresAt = expiresAt
	s.accounts[id] = account
	return nil
}

func (s *memoryStore) MarkReconnectRequired(_ context.Context, id uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.accounts[id]
	if !ok {
		return auth.ErrStreamingAccountNotFound
	}
	account.ReconnectRequiredAt = &at
	s.accounts[id] = account
	return nil
}

func (s *memoryStore) UpdateStreamInfo(_ context.Context, id uuid.UUID, info auth.StreamInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.accounts[id]
	if !ok {
		return auth.ErrStreamingAccountNotFound
	}
	account.StreamID = &info.StreamID
	account.IngestionAddress = &info.IngestionAddress
	account.BackupIngestionAddress = &info.BackupIngestionAddress
	account.RtmpsIngestionAddress = &info.RtmpsIngestionAddress
	account.RtmpsBackupIngestionAddress = &info.RtmpsBackupIngestionAddress
	account.StreamNameCiphertext = info.StreamNameCiphertext
	account.StreamNameKeyVersion = info.StreamNameKeyVersion
	s.accounts[id] = account
	return nil
}

func testCipher(t *testing.T) *auth.ProviderTokenCipher {
	t.Helper()
	cipher, err := auth.NewProviderTokenCipherFromBase64(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

// youtubeAPIStub은 Live API 3종(liveStreams.insert / liveBroadcasts.insert /
// bind)을 흉내 내며 요청을 기록한다.
type youtubeAPIStub struct {
	mu               sync.Mutex
	streamInserts    int
	broadcastInserts int
	binds            int
	lastBroadcast    map[string]any
	bindQuery        string
	blockLive        bool
	// 선택 항목(카테고리·썸네일) 기록·실패 주입.
	videoUpdates       int
	lastVideoUpdate    map[string]any
	videoUpdateStatus  int
	thumbnailUploads   int
	lastThumbnailBody  []byte
	lastThumbnailType  string
	thumbnailSetStatus int
	// 라이브 전환·삭제 기록.
	transitions      int
	transitionQuery  string
	transitionStatus int
	transitionBody   string
	deletes          int
	deleteQuery      string
	// 설정 기본값 조회(#143) 기록·응답 주입.
	broadcastLists      int
	broadcastListQuery  string
	broadcastListBody   string
	broadcastListStatus int
	videoLists          int
	videoListQuery      string
	videoListBody       string
	videoListStatus     int
}

func (s *youtubeAPIStub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/channels"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[{"id":"UCabc","snippet":{"title":"Team Framework Renamed"}}]}`))
		case strings.HasPrefix(r.URL.Path, "/liveStreams"):
			s.streamInserts++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"stream-id-1","cdn":{"ingestionInfo":{
				"streamName":"secret-stream-name",
				"ingestionAddress":"rtmp://a.rtmp.youtube.com/live2",
				"backupIngestionAddress":"rtmp://b.rtmp.youtube.com/live2?backup=1",
				"rtmpsIngestionAddress":"rtmps://a.rtmps.youtube.com/live2",
				"rtmpsBackupIngestionAddress":"rtmps://b.rtmps.youtube.com/live2?backup=1"}}}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/videos"):
			s.videoLists++
			s.videoListQuery = r.URL.RawQuery
			if s.videoListStatus != 0 {
				w.WriteHeader(s.videoListStatus)
				_, _ = w.Write([]byte(`{"error":{"code":403,"message":"forbidden"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(s.videoListBody))
		case strings.HasPrefix(r.URL.Path, "/videos"):
			s.videoUpdates++
			if err := json.NewDecoder(r.Body).Decode(&s.lastVideoUpdate); err != nil {
				t.Errorf("decode videos.update payload: %v", err)
			}
			if s.videoUpdateStatus != 0 {
				w.WriteHeader(s.videoUpdateStatus)
				_, _ = w.Write([]byte(`{"error":{"code":400,"message":"invalid category"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"broadcast-id-1"}`))
		case strings.HasPrefix(r.URL.Path, "/thumbnails/set"):
			s.thumbnailUploads++
			body, _ := io.ReadAll(r.Body)
			s.lastThumbnailBody = body
			s.lastThumbnailType = r.Header.Get("Content-Type")
			if s.thumbnailSetStatus != 0 {
				w.WriteHeader(s.thumbnailSetStatus)
				_, _ = w.Write([]byte(`{"error":{"code":403,"message":"The user is not allowed to upload custom thumbnails."}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[]}`))
		case strings.HasPrefix(r.URL.Path, "/liveBroadcasts/transition"):
			s.transitions++
			s.transitionQuery = r.URL.RawQuery
			if s.transitionStatus != 0 {
				w.WriteHeader(s.transitionStatus)
				_, _ = w.Write([]byte(s.transitionBody))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"broadcast-id-1","status":{"lifeCycleStatus":"live"}}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/liveBroadcasts"):
			s.deletes++
			s.deleteQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/liveBroadcasts/bind"):
			s.binds++
			s.bindQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"broadcast-id-1"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/liveBroadcasts"):
			s.broadcastLists++
			s.broadcastListQuery = r.URL.RawQuery
			if s.broadcastListStatus != 0 {
				w.WriteHeader(s.broadcastListStatus)
				_, _ = w.Write([]byte(`{"error":{"code":403,"message":"forbidden"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(s.broadcastListBody))
		case strings.HasPrefix(r.URL.Path, "/liveBroadcasts"):
			if s.blockLive {
				// 라이브 미활성 채널의 실측 응답 형태(2026-08-09).
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":403,"message":"The user is blocked from live streaming.","errors":[{"message":"The user is blocked from live streaming.","domain":"youtube.liveBroadcast","reason":"livePermissionBlocked","extendedHelp":"https://support.google.com/youtube/answer/2853834"}]}}`))
				return
			}
			s.broadcastInserts++
			if err := json.NewDecoder(r.Body).Decode(&s.lastBroadcast); err != nil {
				t.Errorf("decode broadcast payload: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"broadcast-id-1"}`))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func testProviderWith(t *testing.T, stub *youtubeAPIStub, store auth.StreamingAccountStore) *YouTubeProvider {
	t.Helper()
	server := httptest.NewServer(stub.handler(t))
	t.Cleanup(server.Close)
	provider, err := NewYouTubeProvider(stubTokens{token: "at-value"}, store, testCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	provider.apiBase = server.URL
	provider.uploadBase = server.URL
	return provider
}

func connectedAccount(t *testing.T, store *memoryStore, userID uuid.UUID) auth.StreamingAccount {
	t.Helper()
	account := auth.StreamingAccount{
		ID:        uuid.New(),
		UserID:    userID,
		Provider:  auth.StreamingProviderYouTube,
		ChannelID: "UCabc",
	}
	if err := store.Upsert(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	return account
}

func TestPrepareCreatesReusableStreamOnceAndBindsBroadcast(t *testing.T) {
	stub := &youtubeAPIStub{}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID)
	provider := testProviderWith(t, stub, store)

	first, err := provider.Prepare(context.Background(), userID, PrepareOptions{MadeForKids: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if first.IngestURL != "rtmps://a.rtmps.youtube.com/live2/secret-stream-name" {
		t.Fatalf("IngestURL = %q", first.IngestURL)
	}
	if first.BroadcastID != "broadcast-id-1" || first.StreamID != "stream-id-1" {
		t.Fatalf("prepared = %+v", first)
	}

	// 프리로딩 저장 검증: streamName은 암호문으로만 저장돼야 한다.
	account, err := store.Get(context.Background(), userID, auth.StreamingProviderYouTube)
	if err != nil {
		t.Fatal(err)
	}
	if account.StreamID == nil || *account.StreamID != "stream-id-1" {
		t.Fatalf("stored stream id = %v", account.StreamID)
	}
	if bytes.Contains(account.StreamNameCiphertext, []byte("secret-stream-name")) {
		t.Fatal("stream name stored in plaintext")
	}
	if account.RtmpsIngestionAddress == nil || *account.RtmpsIngestionAddress != "rtmps://a.rtmps.youtube.com/live2" {
		t.Fatalf("stored rtmps address = %v", account.RtmpsIngestionAddress)
	}

	// 두 번째 방송: 저장된 스트림을 재사용해야 한다(liveStreams.insert 1회 유지).
	second, err := provider.Prepare(context.Background(), userID, PrepareOptions{MadeForKids: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if second.IngestURL != first.IngestURL || second.StreamID != first.StreamID {
		t.Fatalf("second prepare = %+v, want stream reuse", second)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.streamInserts != 1 {
		t.Fatalf("liveStreams.insert calls = %d, want 1 (reusable)", stub.streamInserts)
	}
	if stub.broadcastInserts != 2 || stub.binds != 2 {
		t.Fatalf("broadcast/bind calls = %d/%d, want 2/2", stub.broadcastInserts, stub.binds)
	}
	if !strings.Contains(stub.bindQuery, "id=broadcast-id-1") || !strings.Contains(stub.bindQuery, "streamId=stream-id-1") {
		t.Fatalf("bind query = %q", stub.bindQuery)
	}
}

func TestPrepareBroadcastPayloadDefaults(t *testing.T) {
	stub := &youtubeAPIStub{}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID)
	provider := testProviderWith(t, stub, store)

	if _, err := provider.Prepare(context.Background(), userID, PrepareOptions{MadeForKids: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	payload := stub.lastBroadcast
	stub.mu.Unlock()

	content := payload["contentDetails"].(map[string]any)
	monitor := content["monitorStream"].(map[string]any)
	if monitor["enableMonitorStream"] != false {
		t.Fatalf("monitorStream = %v", monitor)
	}
	status := payload["status"].(map[string]any)
	// 기본값은 전체공개가 아니어야 한다(#140).
	if status["privacyStatus"] != "unlisted" {
		t.Fatalf("privacyStatus = %v, want default unlisted", status["privacyStatus"])
	}
	if status["selfDeclaredMadeForKids"] != false {
		t.Fatalf("selfDeclaredMadeForKids = %v, want the requested false", status["selfDeclaredMadeForKids"])
	}
	snippet := payload["snippet"].(map[string]any)
	if snippet["title"] != defaultBroadcastTitle {
		t.Fatalf("title = %v, want default", snippet["title"])
	}

	// 옵션 오버라이드.
	if _, err := provider.Prepare(context.Background(), userID, PrepareOptions{Title: "내 방송", Privacy: "public", MadeForKids: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	payload = stub.lastBroadcast
	stub.mu.Unlock()
	if payload["snippet"].(map[string]any)["title"] != "내 방송" {
		t.Fatalf("title override failed: %v", payload["snippet"])
	}
	if payload["status"].(map[string]any)["privacyStatus"] != "public" {
		t.Fatalf("privacy override failed: %v", payload["status"])
	}
	if payload["status"].(map[string]any)["selfDeclaredMadeForKids"] != true {
		t.Fatalf("made for kids override failed: %v", payload["status"])
	}
}

func boolPtr(value bool) *bool { return &value }

// TestPrepareRejectsMissingMadeForKids: 아동용 여부는 사용자 신고값이라
// 미선택이면 플랫폼 호출 없이 거절해야 한다.
func TestPrepareRejectsMissingMadeForKids(t *testing.T) {
	stub := &youtubeAPIStub{}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID)
	provider := testProviderWith(t, stub, store)

	_, err := provider.Prepare(context.Background(), userID, PrepareOptions{})
	if !errors.Is(err, ErrMadeForKidsRequired) {
		t.Fatalf("error = %v, want ErrMadeForKidsRequired", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.broadcastInserts != 0 || stub.streamInserts != 0 {
		t.Fatalf("YouTube calls = %d/%d, want none", stub.streamInserts, stub.broadcastInserts)
	}
}

func TestPrepareMapsLivePermissionBlocked(t *testing.T) {
	stub := &youtubeAPIStub{blockLive: true}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID)
	provider := testProviderWith(t, stub, store)

	_, err := provider.Prepare(context.Background(), userID, PrepareOptions{MadeForKids: boolPtr(false)})
	if !errors.Is(err, ErrLiveStreamingBlocked) {
		t.Fatalf("error = %v, want ErrLiveStreamingBlocked", err)
	}
}

// TestPrepareRefreshesChannelInfo: 방송 준비가 채널 표시 정보를 갱신해야
// 한다(#88 ④ — 조회 API는 저장값을 반환하므로 여기서 신선도를 맞춘다).
func TestPrepareRefreshesChannelInfo(t *testing.T) {
	stub := &youtubeAPIStub{}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID) // 저장된 제목은 없음(nil), 스텁은 "Team Framework Renamed"를 반환
	provider := testProviderWith(t, stub, store)

	if _, err := provider.Prepare(context.Background(), userID, PrepareOptions{MadeForKids: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	account, err := store.Get(context.Background(), userID, auth.StreamingProviderYouTube)
	if err != nil {
		t.Fatal(err)
	}
	if account.ChannelTitle == nil || *account.ChannelTitle != "Team Framework Renamed" {
		t.Fatalf("channel title = %v, want refreshed value", account.ChannelTitle)
	}
}

// TestCleanupStreamingResources: 저장된 재사용 스트림이 있으면 liveStreams
// DELETE를 부르고, 없으면 API 호출 없이 무동작이어야 한다(#88).
func TestCleanupStreamingResources(t *testing.T) {
	var deletes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || !strings.HasPrefix(r.URL.Path, "/liveStreams") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		deletes = append(deletes, r.URL.Query().Get("id"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	store := newMemoryStore()
	provider, err := NewYouTubeProvider(stubTokens{token: "at-value"}, store, testCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	provider.apiBase = server.URL

	streamID := "stream-id-1"
	withStream := auth.StreamingAccount{ID: uuid.New(), UserID: uuid.New(), Provider: auth.StreamingProviderYouTube, StreamID: &streamID}
	if err := provider.CleanupStreamingResources(context.Background(), withStream); err != nil {
		t.Fatal(err)
	}
	if len(deletes) != 1 || deletes[0] != "stream-id-1" {
		t.Fatalf("deletes = %v, want [stream-id-1]", deletes)
	}

	// 프리로딩된 스트림이 없는 계정은 무동작(추가 API 호출 없음).
	withoutStream := auth.StreamingAccount{ID: uuid.New(), UserID: uuid.New(), Provider: auth.StreamingProviderYouTube}
	if err := provider.CleanupStreamingResources(context.Background(), withoutStream); err != nil {
		t.Fatal(err)
	}
	if len(deletes) != 1 {
		t.Fatalf("deletes = %v, want no additional call", deletes)
	}
}

func TestPrepareRequiresConnection(t *testing.T) {
	provider := testProviderWith(t, &youtubeAPIStub{}, newMemoryStore())
	_, err := provider.Prepare(context.Background(), uuid.New(), PrepareOptions{MadeForKids: boolPtr(false)})
	if !errors.Is(err, auth.ErrStreamingNotConnected) {
		t.Fatalf("error = %v, want ErrStreamingNotConnected", err)
	}
}
