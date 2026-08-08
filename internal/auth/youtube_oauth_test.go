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
		t.Setenv("YOUTUBE_OAUTH_REDIRECT_URI", "")
		config, err := LoadYouTubeOAuthConfigFromEnv()
		if err != nil || config.Enabled() {
			t.Fatalf("config = %+v, err = %v, want disabled without error", config, err)
		}
	})
	t.Run("partial config is an error", func(t *testing.T) {
		t.Setenv("YOUTUBE_OAUTH_CLIENT_ID", "client-id")
		t.Setenv("YOUTUBE_OAUTH_CLIENT_SECRET", "")
		t.Setenv("YOUTUBE_OAUTH_REDIRECT_URI", "")
		if _, err := LoadYouTubeOAuthConfigFromEnv(); err == nil {
			t.Fatal("partial configuration must fail")
		}
	})
	t.Run("complete config enables", func(t *testing.T) {
		t.Setenv("YOUTUBE_OAUTH_CLIENT_ID", "client-id")
		t.Setenv("YOUTUBE_OAUTH_CLIENT_SECRET", "client-secret")
		t.Setenv("YOUTUBE_OAUTH_REDIRECT_URI", "http://localhost:8000/auth/youtube/callback")
		config, err := LoadYouTubeOAuthConfigFromEnv()
		if err != nil || !config.Enabled() {
			t.Fatalf("config = %+v, err = %v, want enabled", config, err)
		}
	})
}

func TestYouTubeAuthorizeURLParameters(t *testing.T) {
	client, err := NewYouTubeOAuthClient(YouTubeOAuthConfig{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURI:  "http://localhost:8000/auth/youtube/callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(client.AuthorizeURL("state-value"))
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	want := map[string]string{
		"client_id":     "client-id",
		"redirect_uri":  "http://localhost:8000/auth/youtube/callback",
		"response_type": "code",
		"scope":         youtubeStreamingScope,
		// prompt=consent 없이는 재승인 시 refresh_token이 빠질 수 있다(실측).
		"access_type": "offline",
		"prompt":      "consent",
		"state":       "state-value",
	}
	for key, value := range want {
		if got := query.Get(key); got != value {
			t.Errorf("authorize URL %s = %q, want %q", key, got, value)
		}
	}
}

// TestYouTubeExchangeEncodesReservedCharacters: 실측에서 인가 코드에 '/'가
// 포함돼 왔다 — form 인코딩이 빠지면 코드가 깨진 채 전송된다.
func TestYouTubeExchangeEncodesReservedCharacters(t *testing.T) {
	const rawCode = "4/0AXtestCODEwith/slash"
	var received url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		received = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-value","refresh_token":"rt-value","expires_in":3599,"refresh_token_expires_in":604799,"scope":"s","token_type":"Bearer"}`))
	}))
	defer server.Close()

	client, err := NewYouTubeOAuthClient(YouTubeOAuthConfig{ClientID: "id", ClientSecret: "secret", RedirectURI: "http://localhost/cb"})
	if err != nil {
		t.Fatal(err)
	}
	client.tokenURL = server.URL
	response, err := client.Exchange(context.Background(), rawCode)
	if err != nil {
		t.Fatal(err)
	}
	if received.Get("code") != rawCode {
		t.Fatalf("server received code %q, want %q", received.Get("code"), rawCode)
	}
	if received.Get("grant_type") != "authorization_code" {
		t.Fatalf("grant_type = %q", received.Get("grant_type"))
	}
	if response.RefreshToken != "rt-value" || response.ExpiresIn != 3599 {
		t.Fatalf("response = %+v", response)
	}
	// Testing 게시 상태에서 오는 필드(실측 604799)를 놓치지 않아야 한다.
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

	client, err := NewYouTubeOAuthClient(YouTubeOAuthConfig{ClientID: "id", ClientSecret: "secret", RedirectURI: "http://localhost/cb"})
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
	mu            sync.Mutex
	exchangeCode  string
	token         YouTubeTokenResponse
	exchangeErr   error
	channel       YouTubeChannel
	channelErr    error
	refreshToken  YouTubeTokenResponse
	refreshErr    error
	refreshCalls  atomic.Int64
	refreshedWith string
}

func (s *stubYouTubeAuthorizer) AuthorizeURL(state string) string {
	return "https://accounts.example/authorize?state=" + state
}

func (s *stubYouTubeAuthorizer) Exchange(_ context.Context, code string) (YouTubeTokenResponse, error) {
	s.mu.Lock()
	s.exchangeCode = code
	s.mu.Unlock()
	return s.token, s.exchangeErr
}

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
	s.accounts[key] = account
	return nil
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

func testYouTubeConnectService(t *testing.T, oauth YouTubeAuthorizer, store StreamingAccountStore, status UserStatus) *YouTubeConnectService {
	t.Helper()
	service, err := NewYouTubeConnectService(oauth, store, testUserStatusChecker{status: status}, testProviderTokenCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestYouTubeConnectIssuesBoundState(t *testing.T) {
	oauth := &stubYouTubeAuthorizer{}
	service := testYouTubeConnectService(t, oauth, newMemoryStreamingAccountStore(), UserStatusActive)
	userID := uuid.New()

	authorizeURL, err := service.Connect(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatal("authorize URL has no state")
	}
	got, ok := service.states.Consume(state)
	if !ok || got != userID {
		t.Fatalf("state binding = (%v, %v), want (%v, true)", got, ok, userID)
	}
}

func TestYouTubeConnectRejectsInactiveUser(t *testing.T) {
	service := testYouTubeConnectService(t, &stubYouTubeAuthorizer{}, newMemoryStreamingAccountStore(), UserStatusDisabled)
	if _, err := service.Connect(context.Background(), uuid.New()); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("Connect error = %v, want ErrUserInactive", err)
	}
}

func TestYouTubeCompleteCallbackPersistsEncryptedConnection(t *testing.T) {
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

	state, err := service.states.Issue(userID)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := service.CompleteCallback(context.Background(), state, "auth-code")
	if err != nil {
		t.Fatal(err)
	}
	if channel.ID != "UCabc" {
		t.Fatalf("channel = %+v", channel)
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
	// Testing 게시 상태의 refresh token 만료(실측 7일)가 추적돼야 한다.
	if account.RefreshTokenExpiresAt == nil {
		t.Fatal("RefreshTokenExpiresAt must be tracked when refresh_token_expires_in is present")
	}
}

func TestYouTubeCompleteCallbackRejectsBadState(t *testing.T) {
	service := testYouTubeConnectService(t, &stubYouTubeAuthorizer{}, newMemoryStreamingAccountStore(), UserStatusActive)
	if _, err := service.CompleteCallback(context.Background(), "unknown-state", "code"); !errors.Is(err, ErrInvalidOAuthState) {
		t.Fatalf("error = %v, want ErrInvalidOAuthState", err)
	}
}

func TestYouTubeCompleteCallbackRequiresRefreshToken(t *testing.T) {
	oauth := &stubYouTubeAuthorizer{token: YouTubeTokenResponse{AccessToken: "at-only"}}
	service := testYouTubeConnectService(t, oauth, newMemoryStreamingAccountStore(), UserStatusActive)
	state, err := service.states.Issue(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteCallback(context.Background(), state, "code"); !errors.Is(err, ErrYouTubeTokenExchange) {
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
