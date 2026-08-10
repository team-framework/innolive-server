package auth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testStreamingAccountsHandler(t *testing.T, store StreamingAccountStore, status UserStatus) (*TokenService, http.Handler) {
	t.Helper()
	config, err := NewTokenHTTPConfig(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens := testTokenService(newMemoryRefreshStore())
	service, err := NewStreamingAccountService(store, testUserStatusChecker{status: status})
	if err != nil {
		t.Fatal(err)
	}
	handler := MountAuthHTTPWithStreaming(http.NotFoundHandler(), tokens, nil, nil, nil, nil, nil, service, slog.New(slog.NewTextHandler(io.Discard, nil)), config)
	return tokens, handler
}

func listStreamingAccounts(t *testing.T, handler http.Handler, accessToken string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/auth/streaming/accounts", nil)
	if accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestListStreamingAccountsRequiresBearer(t *testing.T) {
	_, handler := testStreamingAccountsHandler(t, newMemoryStreamingAccountStore(), UserStatusActive)
	if response := listStreamingAccounts(t, handler, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

// TestListStreamingAccountsEmptyIsArray: 연결이 없으면 404가 아니라 빈 배열
// (JSON `[]`, null 아님)이어야 한다 — 이슈 #88 계약.
func TestListStreamingAccountsEmptyIsArray(t *testing.T) {
	tokens, handler := testStreamingAccountsHandler(t, newMemoryStreamingAccountStore(), UserStatusActive)
	pair, err := tokens.IssuePair(context.Background(), uuid.New(), ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	response := listStreamingAccounts(t, handler, pair.AccessToken)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if body := strings.TrimSpace(response.Body.String()); body != "[]" {
		t.Fatalf("empty list body = %q, want []", body)
	}
}

func TestListStreamingAccountsReturnsConnections(t *testing.T) {
	store := newMemoryStreamingAccountStore()
	userID := uuid.New()
	title := "Team Framework"
	if err := store.Upsert(context.Background(), StreamingAccount{
		UserID:       userID,
		Provider:     StreamingProviderYouTube,
		ChannelID:    "UCabc",
		ChannelTitle: &title,
	}); err != nil {
		t.Fatal(err)
	}
	// 다른 사용자의 연결은 보이면 안 된다.
	if err := store.Upsert(context.Background(), StreamingAccount{
		UserID:    uuid.New(),
		Provider:  StreamingProviderYouTube,
		ChannelID: "UCother",
	}); err != nil {
		t.Fatal(err)
	}

	tokens, handler := testStreamingAccountsHandler(t, store, UserStatusActive)
	pair, err := tokens.IssuePair(context.Background(), userID, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	response := listStreamingAccounts(t, handler, pair.AccessToken)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload []struct {
		Provider          string `json:"provider"`
		ChannelID         string `json:"channel_id"`
		ChannelTitle      string `json:"channel_title"`
		ConnectedAt       string `json:"connected_at"`
		ReconnectRequired bool   `json:"reconnect_required"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 1 {
		t.Fatalf("items = %d, want 1 (own connection only)", len(payload))
	}
	item := payload[0]
	if item.Provider != "youtube" || item.ChannelID != "UCabc" || item.ChannelTitle != "Team Framework" {
		t.Fatalf("item = %+v", item)
	}
	if item.ReconnectRequired {
		t.Fatal("healthy connection must not require reconnect")
	}
	if item.ConnectedAt == "" {
		t.Fatal("connected_at missing")
	}
}

// TestListStreamingAccountsReconnectRequired: 표식·만료 어느 쪽으로든 재연결
// 필요가 조회 응답에 드러나야 한다 — 플랫폼 API 호출 없이.
func TestListStreamingAccountsReconnectRequired(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Hour)

	cases := []struct {
		name    string
		mutate  func(*StreamingAccount)
		require bool
	}{
		{"marked invalid", func(a *StreamingAccount) { a.ReconnectRequiredAt = &past }, true},
		{"testing token expired", func(a *StreamingAccount) { a.RefreshTokenExpiresAt = &past }, true},
		{"testing token still valid", func(a *StreamingAccount) {
			future := now.Add(24 * time.Hour)
			a.RefreshTokenExpiresAt = &future
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStreamingAccountStore()
			userID := uuid.New()
			account := StreamingAccount{UserID: userID, Provider: StreamingProviderYouTube, ChannelID: "UCabc"}
			tc.mutate(&account)
			if err := store.Upsert(context.Background(), account); err != nil {
				t.Fatal(err)
			}
			tokens, handler := testStreamingAccountsHandler(t, store, UserStatusActive)
			pair, err := tokens.IssuePair(context.Background(), userID, ClientInfo{})
			if err != nil {
				t.Fatal(err)
			}
			response := listStreamingAccounts(t, handler, pair.AccessToken)
			var payload []struct {
				ReconnectRequired bool `json:"reconnect_required"`
			}
			if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if len(payload) != 1 || payload[0].ReconnectRequired != tc.require {
				t.Fatalf("payload = %+v, want reconnect_required=%v", payload, tc.require)
			}
		})
	}
}
