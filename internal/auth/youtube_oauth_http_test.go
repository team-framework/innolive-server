package auth

import (
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

func TestYouTubeConnectHTTPRequiresBearer(t *testing.T) {
	service := testYouTubeConnectService(t, &stubYouTubeAuthorizer{}, newMemoryStreamingAccountStore(), UserStatusActive)
	_, handler := testYouTubeHTTPHandler(t, service)

	request := httptest.NewRequest(http.MethodPost, "/auth/youtube/connect", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestYouTubeConnectHTTPReturnsAuthorizeURL(t *testing.T) {
	service := testYouTubeConnectService(t, &stubYouTubeAuthorizer{}, newMemoryStreamingAccountStore(), UserStatusActive)
	tokens, handler := testYouTubeHTTPHandler(t, service)

	userID := uuid.New()
	pair, err := tokens.IssuePair(context.Background(), userID, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/auth/youtube/connect", nil)
	request.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.AuthorizeURL == "" {
		t.Fatal("authorize_url is empty")
	}
}

func TestYouTubeCallbackHTTPFlow(t *testing.T) {
	oauth := &stubYouTubeAuthorizer{
		token:   YouTubeTokenResponse{AccessToken: "at", RefreshToken: "rt", ExpiresIn: 3599},
		channel: YouTubeChannel{ID: "UCabc", Title: "Team Framework"},
	}
	store := newMemoryStreamingAccountStore()
	service := testYouTubeConnectService(t, oauth, store, UserStatusActive)
	_, handler := testYouTubeHTTPHandler(t, service)

	userID := uuid.New()
	state, err := service.states.Issue(userID)
	if err != nil {
		t.Fatal(err)
	}

	// Google 콜백에는 문서에 없는 파라미터(iss — 2026-08-09 실측)가 섞여
	// 온다. 파서는 이를 무시하고 성공해야 한다.
	request := httptest.NewRequest(http.MethodGet, "/auth/youtube/callback?state="+state+"&code=auth-code&iss=https://accounts.google.com&scope=x", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := store.Get(context.Background(), userID, StreamingProviderYouTube); err != nil {
		t.Fatalf("connection not persisted: %v", err)
	}

	// 소비된 state 재사용은 거부돼야 한다.
	replay := httptest.NewRequest(http.MethodGet, "/auth/youtube/callback?state="+state+"&code=auth-code", nil)
	replayResponse := httptest.NewRecorder()
	handler.ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusBadRequest {
		t.Fatalf("replay status = %d, want 400", replayResponse.Code)
	}
}

func TestYouTubeCallbackHTTPErrors(t *testing.T) {
	service := testYouTubeConnectService(t, &stubYouTubeAuthorizer{}, newMemoryStreamingAccountStore(), UserStatusActive)
	_, handler := testYouTubeHTTPHandler(t, service)

	// 사용자가 동의 화면에서 거부한 경우(error=access_denied).
	denied := httptest.NewRequest(http.MethodGet, "/auth/youtube/callback?error=access_denied&state=whatever", nil)
	deniedResponse := httptest.NewRecorder()
	handler.ServeHTTP(deniedResponse, denied)
	if deniedResponse.Code != http.StatusBadRequest {
		t.Fatalf("denied status = %d, want 400", deniedResponse.Code)
	}

	// state/code 누락.
	missing := httptest.NewRequest(http.MethodGet, "/auth/youtube/callback", nil)
	missingResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingResponse, missing)
	if missingResponse.Code != http.StatusBadRequest {
		t.Fatalf("missing params status = %d, want 400", missingResponse.Code)
	}

	// 발급된 적 없는 state.
	unknown := httptest.NewRequest(http.MethodGet, "/auth/youtube/callback?state=unknown&code=code", nil)
	unknownResponse := httptest.NewRecorder()
	handler.ServeHTTP(unknownResponse, unknown)
	if unknownResponse.Code != http.StatusBadRequest {
		t.Fatalf("unknown state status = %d, want 400", unknownResponse.Code)
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
