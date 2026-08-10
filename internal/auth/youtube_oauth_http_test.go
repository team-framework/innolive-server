package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
