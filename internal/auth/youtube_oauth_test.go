package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testProviderTokenCipher(t *testing.T) *ProviderTokenCipher {
	t.Helper()
	cipher, err := NewProviderTokenCipherFromBase64(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

func TestLoadYouTubeOAuthConfigFromEnv(t *testing.T) {
	t.Run("all empty disables", func(t *testing.T) {
		t.Setenv("YOUTUBE_OAUTH_CLIENT_ID", "")
		t.Setenv("YOUTUBE_OAUTH_CLIENT_SECRET", "")
		config, err := LoadYouTubeOAuthConfigFromEnv()
		if err != nil || config.Enabled() {
			t.Fatalf("config = %+v, err = %v, want disabled without error", config, err)
		}
	})
	t.Run("partial credentials is an error", func(t *testing.T) {
		t.Setenv("YOUTUBE_OAUTH_CLIENT_ID", "client-id")
		t.Setenv("YOUTUBE_OAUTH_CLIENT_SECRET", "")
		if _, err := LoadYouTubeOAuthConfigFromEnv(); err == nil {
			t.Fatal("partial credentials must fail")
		}
	})
	t.Run("complete credentials enables", func(t *testing.T) {
		t.Setenv("YOUTUBE_OAUTH_CLIENT_ID", "client-id")
		t.Setenv("YOUTUBE_OAUTH_CLIENT_SECRET", "client-secret")
		config, err := LoadYouTubeOAuthConfigFromEnv()
		if err != nil || !config.Enabled() {
			t.Fatalf("config = %+v, err = %v, want enabled", config, err)
		}
	})
}

// TestCodeSourceExchangeRedirectURI: 출처별 교환 redirect_uri 매핑 —
// 2026-08-10 실측(웹 팝업 코드는 생략 시 "Missing parameter: redirect_uri",
// postmessage로 성공)을 계약으로 고정한다.
func TestCodeSourceExchangeRedirectURI(t *testing.T) {
	if got := CodeSourceNative.exchangeRedirectURI(); got != "" {
		t.Fatalf("native redirect = %q, want omitted", got)
	}
	if got := CodeSourceWebPopup.exchangeRedirectURI(); got != "postmessage" {
		t.Fatalf("web_popup redirect = %q, want postmessage", got)
	}
	if CodeSource("browser").Valid() {
		t.Fatal("unknown code source must be invalid")
	}
	if !CodeSourceNative.Valid() || !CodeSourceWebPopup.Valid() {
		t.Fatal("known code sources must be valid")
	}
}

// TestYouTubeExchangeRedirectURIParameter: redirectURI 인자가 빈 값이면 폼에서
// 생략되고, 값이 있으면 포함되는지 검증한다.
func TestYouTubeExchangeRedirectURIParameter(t *testing.T) {
	const rawCode = "4/0AXtestCODEwith/slash"
	var received url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		received = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-value","refresh_token":"rt-value","expires_in":3599,"scope":"s","token_type":"Bearer"}`))
	}))
	defer server.Close()

	client, err := NewYouTubeOAuthClient(YouTubeOAuthConfig{ClientID: "id", ClientSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	client.tokenURL = server.URL

	t.Run("omitted for native", func(t *testing.T) {
		response, err := client.Exchange(context.Background(), rawCode, CodeSourceNative.exchangeRedirectURI())
		if err != nil {
			t.Fatal(err)
		}
		// 실측에서 코드에 '/'가 포함돼 왔다 — form 인코딩 왕복 검증.
		if received.Get("code") != rawCode {
			t.Fatalf("server received code %q, want %q", received.Get("code"), rawCode)
		}
		if _, present := received["redirect_uri"]; present {
			t.Fatal("redirect_uri must be omitted for native codes")
		}
		if response.AccessToken != "at-value" || response.RefreshToken != "rt-value" {
			t.Fatalf("response = %+v", response)
		}
	})
	t.Run("postmessage for web popup", func(t *testing.T) {
		if _, err := client.Exchange(context.Background(), rawCode, CodeSourceWebPopup.exchangeRedirectURI()); err != nil {
			t.Fatal(err)
		}
		if received.Get("redirect_uri") != "postmessage" {
			t.Fatalf("redirect_uri = %q, want postmessage", received.Get("redirect_uri"))
		}
	})
	if client.WebClientID() != "id" {
		t.Fatalf("WebClientID = %q", client.WebClientID())
	}
}

// TestYouTubeExchangeRejectedCode: Google이 4xx로 거절한 코드는 클라이언트
// 오류(ErrYouTubeAuthCodeRejected)로, 5xx는 게이트웨이 오류로 구분돼야 한다.
func TestYouTubeExchangeRejectedCode(t *testing.T) {
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer rejected.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer broken.Close()

	client, err := NewYouTubeOAuthClient(YouTubeOAuthConfig{ClientID: "id", ClientSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	client.tokenURL = rejected.URL
	if _, err := client.Exchange(context.Background(), "stale-code", ""); !errors.Is(err, ErrYouTubeAuthCodeRejected) {
		t.Fatalf("4xx error = %v, want ErrYouTubeAuthCodeRejected", err)
	}
	client.tokenURL = broken.URL
	if _, err := client.Exchange(context.Background(), "any-code", ""); !errors.Is(err, ErrYouTubeTokenExchange) {
		t.Fatalf("5xx error = %v, want ErrYouTubeTokenExchange", err)
	}
}

func TestYouTubeTokenResponseParsesTestingExpiry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3599,"refresh_token_expires_in":604799,"scope":"s","token_type":"Bearer"}`))
	}))
	defer server.Close()
	client, err := NewYouTubeOAuthClient(YouTubeOAuthConfig{ClientID: "id", ClientSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	client.tokenURL = server.URL
	response, err := client.Exchange(context.Background(), "code", "")
	if err != nil {
		t.Fatal(err)
	}
	// Testing 게시 상태에서만 오는 필드(실측 604799=7일). Production 발급분은
	// 필드 자체가 없다(실측 2026-08-10) — 0으로 파싱되어 "무기한"으로 다룬다.
	if response.RefreshTokenExpiresIn != 604799 {
		t.Fatalf("refresh_token_expires_in = %d, want 604799", response.RefreshTokenExpiresIn)
	}
}

func TestYouTubeChannelForToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer at-value" {
			t.Errorf("authorization header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"UCabc","snippet":{"title":"Team Framework"}}]}`))
	}))
	defer server.Close()
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer empty.Close()

	client, err := NewYouTubeOAuthClient(YouTubeOAuthConfig{ClientID: "id", ClientSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	client.channelsURL = server.URL
	channel, err := client.ChannelForToken(context.Background(), "at-value")
	if err != nil || channel.ID != "UCabc" || channel.Title != "Team Framework" {
		t.Fatalf("channel = %+v, err = %v", channel, err)
	}

	// 채널 없는 Google 계정(실사용 시나리오)은 전용 에러로 구분돼야 한다.
	client.channelsURL = empty.URL
	if _, err := client.ChannelForToken(context.Background(), "at-value"); !errors.Is(err, ErrYouTubeChannelMissing) {
		t.Fatalf("empty items error = %v, want ErrYouTubeChannelMissing", err)
	}
}

// ---- 서비스 계층 테스트 대역 ----

type stubYouTubeAuthorizer struct {
	mu               sync.Mutex
	exchangeCode     string
	exchangeRedirect string
	token            YouTubeTokenResponse
	exchangeErr      error
	channel          YouTubeChannel
	channelErr       error
	refreshToken     YouTubeTokenResponse
	refreshErr       error
	refreshCalls     atomic.Int64
	refreshedWith    string
}

func (s *stubYouTubeAuthorizer) Exchange(_ context.Context, code, redirectURI string) (YouTubeTokenResponse, error) {
	s.mu.Lock()
	s.exchangeCode = code
	s.exchangeRedirect = redirectURI
	s.mu.Unlock()
	return s.token, s.exchangeErr
}

func (s *stubYouTubeAuthorizer) WebClientID() string { return "stub-web-client-id" }

func (s *stubYouTubeAuthorizer) RefreshAccessToken(_ context.Context, refreshToken string) (YouTubeTokenResponse, error) {
	s.refreshCalls.Add(1)
	s.mu.Lock()
	s.refreshedWith = refreshToken
	s.mu.Unlock()
	return s.refreshToken, s.refreshErr
}

func (s *stubYouTubeAuthorizer) ChannelForToken(context.Context, string) (YouTubeChannel, error) {
	return s.channel, s.channelErr
}

type memoryStreamingAccountStore struct {
	mu       sync.Mutex
	accounts map[string]StreamingAccount
}

func newMemoryStreamingAccountStore() *memoryStreamingAccountStore {
	return &memoryStreamingAccountStore{accounts: make(map[string]StreamingAccount)}
}

func streamingKey(userID uuid.UUID, provider StreamingProvider) string {
	return userID.String() + "/" + string(provider)
}

func (s *memoryStreamingAccountStore) Upsert(_ context.Context, account StreamingAccount) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := streamingKey(account.UserID, account.Provider)
	if existing, ok := s.accounts[key]; ok {
		account.ID = existing.ID
	} else if account.ID == uuid.Nil {
		account.ID = uuid.New()
	}
	if account.ConnectedAt.IsZero() {
		account.ConnectedAt = time.Now().UTC()
	}
	s.accounts[key] = account
	return nil
}

func (s *memoryStreamingAccountStore) UpdateChannel(_ context.Context, id uuid.UUID, channelID string, channelTitle *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, account := range s.accounts {
		if account.ID == id {
			account.ChannelID = channelID
			account.ChannelTitle = channelTitle
			s.accounts[key] = account
			return nil
		}
	}
	return ErrStreamingAccountNotFound
}

func (s *memoryStreamingAccountStore) Delete(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, account := range s.accounts {
		if account.ID == id {
			delete(s.accounts, key)
			return nil
		}
	}
	return ErrStreamingAccountNotFound
}

func (s *memoryStreamingAccountStore) ListByUser(_ context.Context, userID uuid.UUID) ([]StreamingAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var accounts []StreamingAccount
	for _, account := range s.accounts {
		if account.UserID == userID {
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}

func (s *memoryStreamingAccountStore) Get(_ context.Context, userID uuid.UUID, provider StreamingProvider) (StreamingAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.accounts[streamingKey(userID, provider)]
	if !ok {
		return StreamingAccount{}, ErrStreamingAccountNotFound
	}
	return account, nil
}

func (s *memoryStreamingAccountStore) UpdateRefreshToken(_ context.Context, id uuid.UUID, ciphertext []byte, version *int16, expiresAt *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, account := range s.accounts {
		if account.ID == id {
			account.RefreshTokenCiphertext = ciphertext
			account.TokenKeyVersion = version
			account.RefreshTokenExpiresAt = expiresAt
			s.accounts[key] = account
			return nil
		}
	}
	return ErrStreamingAccountNotFound
}

func (s *memoryStreamingAccountStore) MarkReconnectRequired(_ context.Context, id uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, account := range s.accounts {
		if account.ID == id {
			account.ReconnectRequiredAt = &at
			s.accounts[key] = account
			return nil
		}
	}
	return ErrStreamingAccountNotFound
}

func (s *memoryStreamingAccountStore) UpdateStreamInfo(_ context.Context, id uuid.UUID, info StreamInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, account := range s.accounts {
		if account.ID == id {
			account.StreamID = &info.StreamID
			account.IngestionAddress = &info.IngestionAddress
			account.BackupIngestionAddress = &info.BackupIngestionAddress
			account.RtmpsIngestionAddress = &info.RtmpsIngestionAddress
			account.RtmpsBackupIngestionAddress = &info.RtmpsBackupIngestionAddress
			account.StreamNameCiphertext = info.StreamNameCiphertext
			account.StreamNameKeyVersion = info.StreamNameKeyVersion
			s.accounts[key] = account
			return nil
		}
	}
	return ErrStreamingAccountNotFound
}

func testYouTubeConnectService(t *testing.T, oauth YouTubeAuthorizer, store StreamingAccountStore, status UserStatus) *YouTubeConnectService {
	t.Helper()
	service, err := NewYouTubeConnectService(oauth, store, testUserStatusChecker{status: status}, testProviderTokenCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestYouTubeConnectWithAuthCodePersistsEncryptedConnection(t *testing.T) {
	oauth := &stubYouTubeAuthorizer{
		token: YouTubeTokenResponse{
			AccessToken:           "at-value",
			RefreshToken:          "rt-secret",
			ExpiresIn:             3599,
			RefreshTokenExpiresIn: 604799,
		},
		channel: YouTubeChannel{ID: "UCabc", Title: "Team Framework"},
	}
	store := newMemoryStreamingAccountStore()
	service := testYouTubeConnectService(t, oauth, store, UserStatusActive)
	userID := uuid.New()

	channel, err := service.ConnectWithAuthCode(context.Background(), userID, "server-auth-code", CodeSourceNative)
	if err != nil {
		t.Fatal(err)
	}
	if channel.ID != "UCabc" {
		t.Fatalf("channel = %+v", channel)
	}
	oauth.mu.Lock()
	exchanged := oauth.exchangeCode
	oauth.mu.Unlock()
	if exchanged != "server-auth-code" {
		t.Fatalf("exchanged code = %q", exchanged)
	}

	account, err := store.Get(context.Background(), userID, StreamingProviderYouTube)
	if err != nil {
		t.Fatal(err)
	}
	if account.ChannelID != "UCabc" || account.ChannelTitle == nil || *account.ChannelTitle != "Team Framework" {
		t.Fatalf("account channel = %q/%v", account.ChannelID, account.ChannelTitle)
	}
	// refresh token은 평문이 아니라 암호문으로 저장돼야 한다.
	if bytes.Contains(account.RefreshTokenCiphertext, []byte("rt-secret")) {
		t.Fatal("refresh token stored in plaintext")
	}
	plaintext, err := testProviderTokenCipher(t).Decrypt(account.RefreshTokenCiphertext, account.TokenKeyVersion)
	if err != nil || plaintext != "rt-secret" {
		t.Fatalf("decrypt = (%q, %v)", plaintext, err)
	}
	// Testing 게시 상태 토큰의 만료(7일)는 재연결 유도 신호로 추적돼야 한다.
	if account.RefreshTokenExpiresAt == nil {
		t.Fatal("RefreshTokenExpiresAt must be tracked when refresh_token_expires_in is present")
	}
}

func TestYouTubeConnectWithAuthCodeProductionTokenHasNoExpiry(t *testing.T) {
	// Production 게시 발급분은 refresh_token_expires_in 부재(실측 2026-08-10)
	// — 만료 추적 컬럼은 NULL이어야 한다.
	oauth := &stubYouTubeAuthorizer{
		token:   YouTubeTokenResponse{AccessToken: "at", RefreshToken: "rt", ExpiresIn: 3599},
		channel: YouTubeChannel{ID: "UCabc"},
	}
	store := newMemoryStreamingAccountStore()
	service := testYouTubeConnectService(t, oauth, store, UserStatusActive)
	userID := uuid.New()
	if _, err := service.ConnectWithAuthCode(context.Background(), userID, "code", CodeSourceNative); err != nil {
		t.Fatal(err)
	}
	account, err := store.Get(context.Background(), userID, StreamingProviderYouTube)
	if err != nil {
		t.Fatal(err)
	}
	if account.RefreshTokenExpiresAt != nil {
		t.Fatalf("RefreshTokenExpiresAt = %v, want nil for production tokens", account.RefreshTokenExpiresAt)
	}
}

func TestYouTubeConnectWithAuthCodeRejectsInactiveUser(t *testing.T) {
	service := testYouTubeConnectService(t, &stubYouTubeAuthorizer{}, newMemoryStreamingAccountStore(), UserStatusDisabled)
	if _, err := service.ConnectWithAuthCode(context.Background(), uuid.New(), "code", CodeSourceNative); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("error = %v, want ErrUserInactive", err)
	}
}

func TestYouTubeConnectWithAuthCodeRequiresRefreshToken(t *testing.T) {
	oauth := &stubYouTubeAuthorizer{token: YouTubeTokenResponse{AccessToken: "at-only"}}
	service := testYouTubeConnectService(t, oauth, newMemoryStreamingAccountStore(), UserStatusActive)
	if _, err := service.ConnectWithAuthCode(context.Background(), uuid.New(), "code", CodeSourceNative); !errors.Is(err, ErrYouTubeTokenExchange) {
		t.Fatalf("error = %v, want ErrYouTubeTokenExchange (missing refresh_token)", err)
	}
}

// TestYouTubeAccessTokenProviderSerializesRefresh: 같은 사용자의 동시 요청이
// 토큰 갱신을 한 번만 트리거하고 모두 같은 토큰을 받아야 한다(다중 방송
// 세션 시나리오).
func TestYouTubeAccessTokenProviderSerializesRefresh(t *testing.T) {
	cipher := testProviderTokenCipher(t)
	ciphertext, version, err := cipher.Encrypt("rt-secret")
	if err != nil {
		t.Fatal(err)
	}
	store := newMemoryStreamingAccountStore()
	userID := uuid.New()
	if err := store.Upsert(context.Background(), StreamingAccount{
		UserID:                 userID,
		Provider:               StreamingProviderYouTube,
		ChannelID:              "UCabc",
		RefreshTokenCiphertext: ciphertext,
		TokenKeyVersion:        version,
	}); err != nil {
		t.Fatal(err)
	}
	oauth := &stubYouTubeAuthorizer{refreshToken: YouTubeTokenResponse{AccessToken: "fresh-at", ExpiresIn: 3600}}
	provider, err := NewYouTubeAccessTokenProvider(oauth, store, cipher)
	if err != nil {
		t.Fatal(err)
	}

	const parallel = 8
	tokens := make([]string, parallel)
	var wait sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			token, err := provider.AccessToken(context.Background(), userID)
			if err != nil {
				t.Errorf("AccessToken: %v", err)
				return
			}
			tokens[index] = token
		}(i)
	}
	wait.Wait()

	if calls := oauth.refreshCalls.Load(); calls != 1 {
		t.Fatalf("refresh calls = %d, want 1 (per-user serialization)", calls)
	}
	for _, token := range tokens {
		if token != "fresh-at" {
			t.Fatalf("tokens = %v, want all %q", tokens, "fresh-at")
		}
	}
	oauth.mu.Lock()
	refreshedWith := oauth.refreshedWith
	oauth.mu.Unlock()
	if refreshedWith != "rt-secret" {
		t.Fatalf("refreshed with %q, want decrypted refresh token", refreshedWith)
	}
}

// TestYouTubeAccessTokenProviderMarksReconnectRequired: refresh token이 토큰
// 엔드포인트에서 4xx로 거절되면(권한 취소·만료) 전용 에러로 구분되고 계정에
// 재연결 필요 표식이 남아야 한다 — 조회 API가 API 호출 없이 판별하는 근거.
func TestYouTubeAccessTokenProviderMarksReconnectRequired(t *testing.T) {
	cipher := testProviderTokenCipher(t)
	ciphertext, version, err := cipher.Encrypt("rt-revoked")
	if err != nil {
		t.Fatal(err)
	}
	store := newMemoryStreamingAccountStore()
	userID := uuid.New()
	if err := store.Upsert(context.Background(), StreamingAccount{
		UserID:                 userID,
		Provider:               StreamingProviderYouTube,
		ChannelID:              "UCabc",
		RefreshTokenCiphertext: ciphertext,
		TokenKeyVersion:        version,
	}); err != nil {
		t.Fatal(err)
	}
	oauth := &stubYouTubeAuthorizer{refreshErr: ErrYouTubeAuthCodeRejected}
	provider, err := NewYouTubeAccessTokenProvider(oauth, store, cipher)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.AccessToken(context.Background(), userID); !errors.Is(err, ErrStreamingReconnectRequired) {
		t.Fatalf("error = %v, want ErrStreamingReconnectRequired", err)
	}
	account, err := store.Get(context.Background(), userID, StreamingProviderYouTube)
	if err != nil {
		t.Fatal(err)
	}
	if account.ReconnectRequiredAt == nil {
		t.Fatal("ReconnectRequiredAt must be marked after an invalid refresh token")
	}

	// 재연결(Upsert)이 표식을 해소해야 한다.
	if err := store.Upsert(context.Background(), StreamingAccount{
		UserID:                 userID,
		Provider:               StreamingProviderYouTube,
		ChannelID:              "UCabc",
		RefreshTokenCiphertext: ciphertext,
		TokenKeyVersion:        version,
	}); err != nil {
		t.Fatal(err)
	}
	account, err = store.Get(context.Background(), userID, StreamingProviderYouTube)
	if err != nil {
		t.Fatal(err)
	}
	if account.ReconnectRequiredAt != nil {
		t.Fatalf("ReconnectRequiredAt = %v after reconnect, want nil", account.ReconnectRequiredAt)
	}
}

func TestYouTubeAccessTokenProviderNotConnected(t *testing.T) {
	provider, err := NewYouTubeAccessTokenProvider(&stubYouTubeAuthorizer{}, newMemoryStreamingAccountStore(), testProviderTokenCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.AccessToken(context.Background(), uuid.New()); !errors.Is(err, ErrStreamingNotConnected) {
		t.Fatalf("error = %v, want ErrStreamingNotConnected", err)
	}
}

// TestYouTubeAccessTokenProviderPersistsRotatedToken: Google이 예외적으로 새
// refresh token을 돌려주면 암호화해 교체 저장해야 한다.
func TestYouTubeAccessTokenProviderPersistsRotatedToken(t *testing.T) {
	cipher := testProviderTokenCipher(t)
	ciphertext, version, err := cipher.Encrypt("rt-old")
	if err != nil {
		t.Fatal(err)
	}
	store := newMemoryStreamingAccountStore()
	userID := uuid.New()
	if err := store.Upsert(context.Background(), StreamingAccount{
		UserID:                 userID,
		Provider:               StreamingProviderYouTube,
		ChannelID:              "UCabc",
		RefreshTokenCiphertext: ciphertext,
		TokenKeyVersion:        version,
	}); err != nil {
		t.Fatal(err)
	}
	oauth := &stubYouTubeAuthorizer{refreshToken: YouTubeTokenResponse{AccessToken: "fresh-at", RefreshToken: "rt-new", ExpiresIn: 3600}}
	provider, err := NewYouTubeAccessTokenProvider(oauth, store, cipher)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.AccessToken(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	account, err := store.Get(context.Background(), userID, StreamingProviderYouTube)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := cipher.Decrypt(account.RefreshTokenCiphertext, account.TokenKeyVersion)
	if err != nil || plaintext != "rt-new" {
		t.Fatalf("stored refresh token = (%q, %v), want rt-new", plaintext, err)
	}
}
