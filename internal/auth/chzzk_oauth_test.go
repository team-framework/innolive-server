package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestChzzkHasScopes: scope 응답은 한글 표시명을 공백으로 이은 문자열이고
// 표시명 안에도 공백이 있다. split으로 가르면 "방송"·"설정"·"조회"로 쪼개져
// 판정이 무너지므로 포함 검사여야 한다.
func TestChzzkHasScopes(t *testing.T) {
	granted := strings.Join([]string{
		ChzzkScopeUserRead, ChzzkScopeStreamKeyRead,
		ChzzkScopeLiveSettingRead, ChzzkScopeLiveSettingWrite,
	}, " ")

	ok, missing := ChzzkHasScopes(granted, ChzzkRequiredScopes)
	if !ok || len(missing) != 0 {
		t.Fatalf("ok = %v, missing = %v, want all four recognized in %q", ok, missing, granted)
	}
	// 공백 split이었다면 토큰 수가 4개가 아니라 9개다 — 계약을 고정해 둔다.
	if fields := strings.Fields(granted); len(fields) == len(ChzzkRequiredScopes) {
		t.Fatalf("scope names are expected to contain spaces; fields = %v", fields)
	}
}

func TestChzzkHasScopesReportsMissing(t *testing.T) {
	granted := ChzzkScopeLiveSettingRead + " " + ChzzkScopeLiveSettingWrite
	ok, missing := ChzzkHasScopes(granted, ChzzkRequiredScopes)
	if ok {
		t.Fatal("partial grant must not pass")
	}
	if len(missing) != 2 || missing[0] != ChzzkScopeUserRead || missing[1] != ChzzkScopeStreamKeyRead {
		t.Fatalf("missing = %v, want the two ungranted scopes", missing)
	}
}

func TestLoadChzzkOAuthConfigFromEnv(t *testing.T) {
	t.Run("all empty disables", func(t *testing.T) {
		t.Setenv("CHZZK_OAUTH_CLIENT_ID", "")
		t.Setenv("CHZZK_OAUTH_CLIENT_SECRET", "")
		t.Setenv("CHZZK_OAUTH_REDIRECT_URI", "")
		config, err := LoadChzzkOAuthConfigFromEnv()
		if err != nil || config.Enabled() {
			t.Fatalf("config enabled = %v, err = %v, want disabled without error", config.Enabled(), err)
		}
	})
	t.Run("partial configuration is an error", func(t *testing.T) {
		t.Setenv("CHZZK_OAUTH_CLIENT_ID", "client-id")
		t.Setenv("CHZZK_OAUTH_CLIENT_SECRET", "client-secret")
		t.Setenv("CHZZK_OAUTH_REDIRECT_URI", "")
		if _, err := LoadChzzkOAuthConfigFromEnv(); err == nil {
			t.Fatal("missing redirect URI must fail")
		}
	})
}

// chzzkStub은 치지직 공통 응답 봉투를 흉내내는 테스트 서버다.
type chzzkStub struct {
	server *httptest.Server

	scope          string
	channelID      string
	channelName    string
	rejectExchange bool
	rejectRevoke   bool
	rejectSearch   bool

	exchanges     int
	refreshes     int
	revokes       int
	searches      int
	lastRefresh   string
	issuedRefresh []string

	lastSearchQuery string
	lastSearchSize  string
}

func newChzzkStub(t *testing.T) *chzzkStub {
	t.Helper()
	stub := &chzzkStub{
		scope: strings.Join([]string{
			ChzzkScopeUserRead, ChzzkScopeStreamKeyRead,
			ChzzkScopeLiveSettingRead, ChzzkScopeLiveSettingWrite,
		}, " "),
		channelID:   "channel-1",
		channelName: "테스트 채널",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/v1/token", func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]string{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode token request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch payload["grantType"] {
		case "authorization_code":
			stub.exchanges++
			if stub.rejectExchange {
				_, _ = w.Write([]byte(`{"code":400,"message":"invalid code"}`))
				return
			}
		case "refresh_token":
			stub.refreshes++
			stub.lastRefresh = payload["refreshToken"]
		default:
			t.Errorf("unexpected grantType %q", payload["grantType"])
		}
		// refresh는 일회용이라 호출마다 새 값을 준다.
		next := "refresh-" + time.Now().Format("150405.000000000")
		stub.issuedRefresh = append(stub.issuedRefresh, next)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"content": map[string]any{
				"accessToken":  "access-token",
				"refreshToken": next,
				"tokenType":    "Bearer",
				"expiresIn":    86400,
				"scope":        stub.scope,
			},
		})
	})
	mux.HandleFunc("/open/v1/users/me", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("Authorization = %q, want a space after Bearer", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"content": map[string]any{
				"channelId":   stub.channelID,
				"channelName": stub.channelName,
			},
		})
	})
	// 카테고리 검색만 Client 인증이다 — Bearer가 아니라 Client-Id/Client-Secret을 본다.
	mux.HandleFunc("/open/v1/categories/search", func(w http.ResponseWriter, r *http.Request) {
		stub.searches++
		stub.lastSearchQuery = r.URL.Query().Get("query")
		stub.lastSearchSize = r.URL.Query().Get("size")
		if r.Header.Get("Client-Id") == "" || r.Header.Get("Client-Secret") == "" {
			t.Error("category search must authenticate with Client-Id and Client-Secret headers")
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("category search must not send a user access token")
		}
		w.Header().Set("Content-Type", "application/json")
		if stub.rejectSearch {
			// 실패도 HTTP 200 + 봉투 code로 온다.
			_, _ = w.Write([]byte(`{"code":401,"message":"INVALID_CLIENT"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"content": map[string]any{
				"data": []map[string]any{
					{"categoryType": "GAME", "categoryId": "League_of_Legends", "categoryValue": "리그 오브 레전드", "posterImageUrl": "https://example.test/lol.png"},
					// posterImageUrl은 null이 올 수 있다(실측).
					{"categoryType": "GAME", "categoryId": "Marimo_League", "categoryValue": "마리모 리그", "posterImageUrl": nil},
				},
			},
		})
	})
	mux.HandleFunc("/auth/v1/token/revoke", func(w http.ResponseWriter, _ *http.Request) {
		stub.revokes++
		w.Header().Set("Content-Type", "application/json")
		if stub.rejectRevoke {
			// 치지직은 실패도 HTTP 200으로 준다 — 상태 코드로는 구분되지 않는다.
			_, _ = w.Write([]byte(`{"code":400,"message":"invalid client"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":200,"message":null}`))
	})
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *chzzkStub) client() *chzzkOAuthClient {
	return &chzzkOAuthClient{
		config:        ChzzkOAuthConfig{ClientID: "id", ClientSecret: "secret", RedirectURI: "https://example.test/cb"},
		httpClient:    s.server.Client(),
		authorizeURL:  chzzkAuthorizeEndpoint,
		tokenURL:      s.server.URL + "/auth/v1/token",
		revokeURL:     s.server.URL + "/auth/v1/token/revoke",
		usersMeURL:    s.server.URL + "/open/v1/users/me",
		categoriesURL: s.server.URL + "/open/v1/categories/search",
	}
}

// TestChzzkConnectStoresAccount: 정상 연결 경로 — 토큰 교환, 채널 조회,
// 암호화 저장까지.
func TestChzzkConnectStoresAccount(t *testing.T) {
	stub := newChzzkStub(t)
	store := newMemoryStreamingAccountStore()
	cipher := testProviderTokenCipher(t)
	service, err := NewChzzkConnectService(stub.client(), store, testUserStatusChecker{status: UserStatusActive}, cipher)
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.New()

	channel, err := service.ConnectWithAuthCode(context.Background(), userID, "auth-code", "state-value")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if channel.ID != "channel-1" || channel.Name != "테스트 채널" {
		t.Fatalf("channel = %+v", channel)
	}
	account, err := store.Get(context.Background(), userID, StreamingProviderChzzk)
	if err != nil {
		t.Fatalf("stored account: %v", err)
	}
	if account.ChannelTitle == nil || *account.ChannelTitle != "테스트 채널" {
		t.Fatalf("channel title = %v", account.ChannelTitle)
	}
	if len(account.RefreshTokenCiphertext) == 0 {
		t.Fatal("refresh token must be stored encrypted")
	}
	plaintext, err := cipher.Decrypt(account.RefreshTokenCiphertext, account.TokenKeyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if plaintext != stub.issuedRefresh[len(stub.issuedRefresh)-1] {
		t.Fatalf("stored refresh token does not match the issued one")
	}
	// refresh 만료는 응답이 주지 않으므로 수명 상수로 기록된다.
	if account.RefreshTokenExpiresAt == nil || time.Until(*account.RefreshTokenExpiresAt) < 29*24*time.Hour {
		t.Fatalf("refresh expiry = %v, want about 30 days out", account.RefreshTokenExpiresAt)
	}
}

// TestChzzkConnectRejectsInvalidCode: 플랫폼이 code를 거절하면 연결이
// 저장되지 않는다.
func TestChzzkConnectRejectsInvalidCode(t *testing.T) {
	stub := newChzzkStub(t)
	stub.rejectExchange = true
	store := newMemoryStreamingAccountStore()
	service, err := NewChzzkConnectService(stub.client(), store, testUserStatusChecker{status: UserStatusActive}, testProviderTokenCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.New()

	if _, err := service.ConnectWithAuthCode(context.Background(), userID, "bad-code", "state"); !errors.Is(err, ErrChzzkAuthCodeRejected) {
		t.Fatalf("error = %v, want ErrChzzkAuthCodeRejected", err)
	}
	if _, err := store.Get(context.Background(), userID, StreamingProviderChzzk); !errors.Is(err, ErrStreamingAccountNotFound) {
		t.Fatalf("a rejected code must not leave an account behind (err = %v)", err)
	}
}

// TestChzzkConnectRejectsPartialScopes: 권한을 일부만 준 동의는 연결 단계에서
// 거절한다 — 저장하면 방송 직전에 실패한다.
func TestChzzkConnectRejectsPartialScopes(t *testing.T) {
	stub := newChzzkStub(t)
	stub.scope = ChzzkScopeLiveSettingRead + " " + ChzzkScopeLiveSettingWrite
	store := newMemoryStreamingAccountStore()
	service, err := NewChzzkConnectService(stub.client(), store, testUserStatusChecker{status: UserStatusActive}, testProviderTokenCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.New()

	if _, err := service.ConnectWithAuthCode(context.Background(), userID, "code", "state"); !errors.Is(err, ErrChzzkScopeMissing) {
		t.Fatalf("error = %v, want ErrChzzkScopeMissing", err)
	}
	if _, err := store.Get(context.Background(), userID, StreamingProviderChzzk); !errors.Is(err, ErrStreamingAccountNotFound) {
		t.Fatal("a partial grant must not be stored")
	}
}

// TestChzzkAccessTokenRotatesRefreshToken: 치지직 refresh는 일회용이다.
// 갱신 응답의 새 값이 저장돼야 하고, 이전 값이 남아 있으면 안 된다.
func TestChzzkAccessTokenRotatesRefreshToken(t *testing.T) {
	stub := newChzzkStub(t)
	store := newMemoryStreamingAccountStore()
	cipher := testProviderTokenCipher(t)
	connect, err := NewChzzkConnectService(stub.client(), store, testUserStatusChecker{status: UserStatusActive}, cipher)
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.New()
	if _, err := connect.ConnectWithAuthCode(context.Background(), userID, "code", "state"); err != nil {
		t.Fatal(err)
	}
	connected, err := store.Get(context.Background(), userID, StreamingProviderChzzk)
	if err != nil {
		t.Fatal(err)
	}
	firstRefresh, err := cipher.Decrypt(connected.RefreshTokenCiphertext, connected.TokenKeyVersion)
	if err != nil {
		t.Fatal(err)
	}

	provider, err := NewChzzkAccessTokenProvider(stub.client(), store, cipher)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.AccessToken(context.Background(), userID); err != nil {
		t.Fatalf("access token: %v", err)
	}
	if stub.lastRefresh != firstRefresh {
		t.Fatalf("refresh call used %q, want the stored token %q", stub.lastRefresh, firstRefresh)
	}
	rotated, err := store.Get(context.Background(), userID, StreamingProviderChzzk)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := cipher.Decrypt(rotated.RefreshTokenCiphertext, rotated.TokenKeyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if stored == firstRefresh {
		t.Fatal("the single-use refresh token was not replaced")
	}
	if stored != stub.issuedRefresh[len(stub.issuedRefresh)-1] {
		t.Fatalf("stored refresh token is not the newest issued one")
	}
	// 계정 행은 교체되는 것이지 새로 생기지 않는다.
	if rotated.ID != connected.ID {
		t.Fatalf("account row changed: %v -> %v", connected.ID, rotated.ID)
	}
}

// TestChzzkRevokeTokenOnDisconnect: 해제 훅이 부르는 revoke가 플랫폼에
// 도달하고, 이미 무효한 토큰이어도 에러로 올리지 않는다.
func TestChzzkRevokeTokenOnDisconnect(t *testing.T) {
	stub := newChzzkStub(t)
	client := stub.client()

	if err := client.RevokeToken(context.Background(), "refresh-value"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if stub.revokes != 1 {
		t.Fatalf("revoke calls = %d, want 1", stub.revokes)
	}
}

// TestChzzkConnectRefusesDuringWithdrawal: 탈퇴 진행 중에는 새 연결이
// 저장되면 안 된다. 저장되면 CleanupForWithdrawal이 이미 지나간 뒤라
// revoke 없이 행만 지워지고, 사용자의 치지직 계정에는 권한이 남는다.
func TestChzzkConnectRefusesDuringWithdrawal(t *testing.T) {
	stub := newChzzkStub(t)
	store := newMemoryStreamingAccountStore()
	service, err := NewChzzkConnectService(stub.client(), store, testUserStatusChecker{status: UserStatusActive}, testProviderTokenCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	service.SetUserOperationGate(blockedOperationGate{})
	userID := uuid.New()

	if _, err := service.ConnectWithAuthCode(context.Background(), userID, "code", "state"); !errors.Is(err, ErrWithdrawalInProgress) {
		t.Fatalf("error = %v, want ErrWithdrawalInProgress", err)
	}
	if stub.exchanges != 0 {
		t.Fatalf("exchange calls = %d, want the gate to refuse before the platform round-trip", stub.exchanges)
	}
	if _, err := store.Get(context.Background(), userID, StreamingProviderChzzk); !errors.Is(err, ErrStreamingAccountNotFound) {
		t.Fatal("no account may be stored while withdrawal is in progress")
	}
}

// blockedOperationGate는 탈퇴가 사용자의 배타 슬롯을 쥐고 있는 상태다.
type blockedOperationGate struct{}

func (blockedOperationGate) BeginOperation(uuid.UUID) (func(), bool) { return func() {}, false }

// TestChzzkRevokeReportsEnvelopeFailure: 치지직이 HTTP 200에 실패 봉투를
// 실어 보내면 revoke는 실패로 올라와야 한다. HTTP 상태만 보면 권한이 안
// 풀렸는데 풀렸다고 보고하게 된다.
func TestChzzkRevokeReportsEnvelopeFailure(t *testing.T) {
	stub := newChzzkStub(t)
	stub.rejectRevoke = true

	err := stub.client().RevokeToken(context.Background(), "refresh-value")
	if err == nil {
		t.Fatal("a failing revoke envelope must surface as an error")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("error = %v, want the platform code reported", err)
	}
}

// TestChzzkRevokeTreatsInvalidTokenAsDone: 이미 무효한 토큰(401)은 해제
// 관점에서 목적이 달성된 상태라 에러로 올리지 않는다.
func TestChzzkRevokeTreatsInvalidTokenAsDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":401,"message":"expired"}`))
	}))
	defer server.Close()
	client := &chzzkOAuthClient{
		config:     ChzzkOAuthConfig{ClientID: "id", ClientSecret: "secret", RedirectURI: "https://example.test/cb"},
		httpClient: server.Client(),
		revokeURL:  server.URL,
	}
	if err := client.RevokeToken(context.Background(), "stale"); err != nil {
		t.Fatalf("an already-invalid token must not fail the disconnect: %v", err)
	}
}

// TestChzzkChannelLookupFailureIsPlatformError: 플랫폼 장애는 우리 결함이
// 아니라 ErrChzzkPlatformUnavailable로 분류돼야 한다 — HTTP 계층이 이걸로
// 500이 아닌 502를 고른다.
func TestChzzkChannelLookupFailureIsPlatformError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer server.Close()
	client := &chzzkOAuthClient{
		config:     ChzzkOAuthConfig{ClientID: "id", ClientSecret: "secret", RedirectURI: "https://example.test/cb"},
		httpClient: server.Client(),
		usersMeURL: server.URL,
	}
	if _, err := client.ChannelForToken(context.Background(), "access"); !errors.Is(err, ErrChzzkPlatformUnavailable) {
		t.Fatalf("error = %v, want ErrChzzkPlatformUnavailable", err)
	}
}

// TestChzzkAuthorizeURL: 인가 URL에 공개 값 셋이 실린다. secret은 실리지 않는다.
func TestChzzkAuthorizeURL(t *testing.T) {
	client, err := NewChzzkOAuthClient(ChzzkOAuthConfig{ClientID: "client-id", ClientSecret: "client-secret", RedirectURI: "https://example.test/cb"})
	if err != nil {
		t.Fatal(err)
	}
	got := client.AuthorizeURL("state-123")
	for _, want := range []string{"clientId=client-id", "state=state-123", "redirectUri=https%3A%2F%2Fexample.test%2Fcb"} {
		if !strings.Contains(got, want) {
			t.Fatalf("authorize URL %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "client-secret") {
		t.Fatal("authorize URL must not carry the client secret")
	}
}
