package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func testYouTubeHTTPHandler(t *testing.T, service *YouTubeConnectService) (*TokenService, http.Handler) {
	t.Helper()
	config, err := NewTokenHTTPConfig(false, []string{"http://localhost:3000"})
	if err != nil {
		t.Fatal(err)
	}
	tokens := testTokenService(newMemoryRefreshStore())
	handler := MountAuthHTTPWithServices(http.NotFoundHandler(), tokens, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), config, service)
	return tokens, handler
}

// testYouTubeHTTPHandlerWithLogs는 로그를 검사해야 하는 테스트용이다.
func testYouTubeHTTPHandlerWithLogs(t *testing.T, service *YouTubeConnectService, logs *bytes.Buffer) (*TokenService, http.Handler) {
	t.Helper()
	config, err := NewTokenHTTPConfig(false, []string{"http://localhost:3000"})
	if err != nil {
		t.Fatal(err)
	}
	tokens := testTokenService(newMemoryRefreshStore())
	handler := MountAuthHTTPWithServices(http.NotFoundHandler(), tokens, nil, nil, nil, nil, slog.New(slog.NewTextHandler(logs, nil)), config, service)
	return tokens, handler
}

func youtubeConnectRequest(t *testing.T, accessToken, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/auth/youtube/connect", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	if accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	return request
}

func TestYouTubeConnectHTTPRequiresBearer(t *testing.T) {
	service := testYouTubeConnectService(t, &stubYouTubeAuthorizer{}, newMemoryStreamingAccountStore(), UserStatusActive)
	_, handler := testYouTubeHTTPHandler(t, service)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, youtubeConnectRequest(t, "", `{"server_auth_code":"code"}`))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestYouTubeConnectHTTPCompletesConnection(t *testing.T) {
	oauth := &stubYouTubeAuthorizer{
		token:   YouTubeTokenResponse{AccessToken: "at", RefreshToken: "rt", ExpiresIn: 3599},
		channel: YouTubeChannel{ID: "UCabc", Title: "Team Framework"},
	}
	store := newMemoryStreamingAccountStore()
	service := testYouTubeConnectService(t, oauth, store, UserStatusActive)
	tokens, handler := testYouTubeHTTPHandler(t, service)

	userID := uuid.New()
	pair, err := tokens.IssuePair(context.Background(), userID, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, youtubeConnectRequest(t, pair.AccessToken, `{"server_auth_code":"4/0AXserver-code"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Connected bool   `json:"connected"`
		Provider  string `json:"provider"`
		Channel   struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"channel"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Connected || payload.Provider != "youtube" || payload.Channel.ID != "UCabc" {
		t.Fatalf("payload = %+v", payload)
	}
	if _, err := store.Get(context.Background(), userID, StreamingProviderYouTube); err != nil {
		t.Fatalf("connection not persisted: %v", err)
	}
}

func TestYouTubeConnectHTTPBadRequests(t *testing.T) {
	service := testYouTubeConnectService(t, &stubYouTubeAuthorizer{}, newMemoryStreamingAccountStore(), UserStatusActive)
	tokens, handler := testYouTubeHTTPHandler(t, service)
	pair, err := tokens.IssuePair(context.Background(), uuid.New(), ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]string{
		"empty body":    ``,
		"missing code":  `{}`,
		"blank code":    `{"server_auth_code":"  "}`,
		"unknown field": `{"server_auth_code":"x","extra":true}`,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, youtubeConnectRequest(t, pair.AccessToken, body))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body: %s)", name, response.Code, response.Body.String())
		}
	}
}

func TestYouTubeConnectHTTPMapsExchangeErrors(t *testing.T) {
	cases := []struct {
		name       string
		stub       *stubYouTubeAuthorizer
		wantStatus int
		wantCode   string
	}{
		{
			name:       "rejected code",
			stub:       &stubYouTubeAuthorizer{exchangeErr: ErrYouTubeAuthCodeRejected},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_auth_code",
		},
		{
			name:       "google outage",
			stub:       &stubYouTubeAuthorizer{exchangeErr: ErrYouTubeTokenExchange},
			wantStatus: http.StatusBadGateway,
			wantCode:   "youtube_token_exchange_failed",
		},
		{
			name: "channel missing",
			stub: &stubYouTubeAuthorizer{
				token:      YouTubeTokenResponse{AccessToken: "at", RefreshToken: "rt"},
				channelErr: ErrYouTubeChannelMissing,
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "youtube_channel_missing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := testYouTubeConnectService(t, tc.stub, newMemoryStreamingAccountStore(), UserStatusActive)
			tokens, handler := testYouTubeHTTPHandler(t, service)
			pair, err := tokens.IssuePair(context.Background(), uuid.New(), ClientInfo{})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, youtubeConnectRequest(t, pair.AccessToken, `{"server_auth_code":"code"}`))
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", response.Code, tc.wantStatus, response.Body.String())
			}
			var payload struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Error.Code != tc.wantCode {
				t.Fatalf("error code = %q, want %q", payload.Error.Code, tc.wantCode)
			}
		})
	}
}

// TestYouTubeConnectHTTPCodeSource: code_source가 교환 redirect_uri 매핑으로
// 이어지고(web_popup→postmessage 실측 계약), 미지 값은 400이어야 한다.
func TestYouTubeConnectHTTPCodeSource(t *testing.T) {
	oauth := &stubYouTubeAuthorizer{
		token:   YouTubeTokenResponse{AccessToken: "at", RefreshToken: "rt", ExpiresIn: 3599},
		channel: YouTubeChannel{ID: "UCabc"},
	}
	service := testYouTubeConnectService(t, oauth, newMemoryStreamingAccountStore(), UserStatusActive)
	tokens, handler := testYouTubeHTTPHandler(t, service)
	pair, err := tokens.IssuePair(context.Background(), uuid.New(), ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, youtubeConnectRequest(t, pair.AccessToken, `{"server_auth_code":"c","code_source":"web_popup"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("web_popup status = %d, body = %s", response.Code, response.Body.String())
	}
	oauth.mu.Lock()
	redirect := oauth.exchangeRedirect
	oauth.mu.Unlock()
	if redirect != "postmessage" {
		t.Fatalf("web_popup exchanged with redirect %q, want postmessage", redirect)
	}

	// 생략 시 native(하위호환) — redirect_uri 생략.
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, youtubeConnectRequest(t, pair.AccessToken, `{"server_auth_code":"c2"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("default source status = %d, body = %s", response.Code, response.Body.String())
	}
	oauth.mu.Lock()
	redirect = oauth.exchangeRedirect
	oauth.mu.Unlock()
	if redirect != "" {
		t.Fatalf("native exchanged with redirect %q, want omitted", redirect)
	}

	// 미지 출처는 400.
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, youtubeConnectRequest(t, pair.AccessToken, `{"server_auth_code":"c3","code_source":"browser"}`))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown source status = %d, want 400", response.Code)
	}
}

// TestYouTubeConfigHTTPExposesWebClientID: 웹 클라이언트가 GIS 초기화에 쓸
// 공개 설정 — 인증 없이 접근 가능해야 한다.
func TestYouTubeConfigHTTPExposesWebClientID(t *testing.T) {
	service := testYouTubeConnectService(t, &stubYouTubeAuthorizer{}, newMemoryStreamingAccountStore(), UserStatusActive)
	_, handler := testYouTubeHTTPHandler(t, service)

	request := httptest.NewRequest(http.MethodGet, "/auth/youtube/config", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		WebClientID string `json:"web_client_id"`
		Scope       string `json:"scope"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.WebClientID != "stub-web-client-id" || payload.Scope != YouTubeStreamingScope {
		t.Fatalf("payload = %+v", payload)
	}
}

// TestYouTubeCallbackRouteRemoved: serverAuthCode 전환으로 브라우저 콜백
// 라우트는 존재하지 않아야 한다(다음 핸들러로 흘러 404).
func TestYouTubeCallbackRouteRemoved(t *testing.T) {
	service := testYouTubeConnectService(t, &stubYouTubeAuthorizer{}, newMemoryStreamingAccountStore(), UserStatusActive)
	_, handler := testYouTubeHTTPHandler(t, service)
	request := httptest.NewRequest(http.MethodGet, "/auth/youtube/callback?state=x&code=y", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (callback route must not exist)", response.Code)
	}
}

// TestYouTubeRoutesAbsentWhenServiceNil: YouTube 서비스가 조립되지 않은
// 배포에서는 라우트 자체가 등록되지 않아야 한다(기존 mount 계약 보존).
func TestYouTubeRoutesAbsentWhenServiceNil(t *testing.T) {
	config, err := NewTokenHTTPConfig(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens := testTokenService(newMemoryRefreshStore())
	handler := MountAuthHTTPWithServices(http.NotFoundHandler(), tokens, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), config)

	request := httptest.NewRequest(http.MethodPost, "/auth/youtube/connect", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when service is absent", response.Code)
	}
}

// 연동 실패가 로그에 남지 않으면 원인을 서버에서 특정할 수 없다. 클라이언트는
// `연결 실패`라는 뭉뚱그린 문구만 보여준다.
func TestYouTubeConnectHTTPLogsFailures(t *testing.T) {
	const authCode = "4/0AXsecret-authorization-code"
	cases := []struct {
		name       string
		stub       *stubYouTubeAuthorizer
		body       string
		wantReason string
		wantLevel  string
	}{
		{
			name:       "rejected code",
			stub:       &stubYouTubeAuthorizer{exchangeErr: ErrYouTubeAuthCodeRejected},
			body:       `{"server_auth_code":"` + authCode + `"}`,
			wantReason: "auth_code_rejected",
			wantLevel:  "WARN",
		},
		{
			name:       "channel missing",
			stub:       &stubYouTubeAuthorizer{token: YouTubeTokenResponse{AccessToken: "at", RefreshToken: "rt"}, channelErr: ErrYouTubeChannelMissing},
			body:       `{"server_auth_code":"` + authCode + `"}`,
			wantReason: "channel_missing",
			wantLevel:  "WARN",
		},
		{
			name:      "token exchange failed",
			stub:      &stubYouTubeAuthorizer{exchangeErr: ErrYouTubeTokenExchange},
			body:      `{"server_auth_code":"` + authCode + `"}`,
			wantLevel: "ERROR",
		},
		{
			name:       "malformed request",
			stub:       &stubYouTubeAuthorizer{},
			body:       `{"server_auth_code":""}`,
			wantReason: "invalid_request",
			wantLevel:  "WARN",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			service := testYouTubeConnectService(t, tc.stub, newMemoryStreamingAccountStore(), UserStatusActive)
			tokens, handler := testYouTubeHTTPHandlerWithLogs(t, service, &logs)
			pair, err := tokens.IssuePair(context.Background(), uuid.New(), ClientInfo{})
			if err != nil {
				t.Fatal(err)
			}
			handler.ServeHTTP(httptest.NewRecorder(), youtubeConnectRequest(t, pair.AccessToken, tc.body))

			got := logs.String()
			if !strings.Contains(got, tc.wantLevel) {
				t.Fatalf("level %s missing: %q", tc.wantLevel, got)
			}
			if tc.wantReason != "" && !strings.Contains(got, tc.wantReason) {
				t.Fatalf("reason %s missing: %q", tc.wantReason, got)
			}
			// 인가 코드는 자격증명이라 어떤 분기에서도 로그에 남으면 안 된다.
			if strings.Contains(got, authCode) {
				t.Fatalf("authorization code leaked into logs: %q", got)
			}
		})
	}
}

// 인증 실패도 남아야 한다 — 토큰 만료와 헤더 누락은 클라이언트 버그 추적의 출발점이다.
func TestYouTubeConnectHTTPLogsMissingBearer(t *testing.T) {
	var logs bytes.Buffer
	service := testYouTubeConnectService(t, &stubYouTubeAuthorizer{}, newMemoryStreamingAccountStore(), UserStatusActive)
	_, handler := testYouTubeHTTPHandlerWithLogs(t, service, &logs)

	handler.ServeHTTP(httptest.NewRecorder(), youtubeConnectRequest(t, "", `{"server_auth_code":"code"}`))
	if got := logs.String(); !strings.Contains(got, "missing_bearer_token") {
		t.Fatalf("missing bearer was not logged: %q", got)
	}
}
