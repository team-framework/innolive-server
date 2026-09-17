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

func testChzzkHandler(t *testing.T, stub *chzzkStub, store StreamingAccountStore) (*TokenService, http.Handler, *chzzkOAuthClient) {
	t.Helper()
	config, err := NewTokenHTTPConfig(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens := testTokenService(newMemoryRefreshStore())
	client := stub.client()
	connect, err := NewChzzkConnectService(client, store, testUserStatusChecker{status: UserStatusActive}, testProviderTokenCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	// 해제 훅은 프로덕션 조립(main)과 같은 모양으로 건다 — revoke만 있고
	// 원격 리소스 정리는 없다.
	accounts, err := NewStreamingAccountService(
		store,
		testUserStatusChecker{status: UserStatusActive},
		testProviderTokenCipher(t),
		map[StreamingProvider]StreamingDisconnectHooks{
			StreamingProviderChzzk: {RevokeToken: client.RevokeToken},
		},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	handler := MountAuthHTTPWithStreamingProviders(
		http.NotFoundHandler(), tokens, nil, nil, nil, nil, nil, connect, accounts,
		slog.New(slog.NewTextHandler(io.Discard, nil)), config,
	)
	return tokens, handler, client
}

func postChzzkConnect(t *testing.T, handler http.Handler, accessToken, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/auth/chzzk/connect", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	if accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestChzzkConnectRequiresBearer(t *testing.T) {
	_, handler, _ := testChzzkHandler(t, newChzzkStub(t), newMemoryStreamingAccountStore())
	if response := postChzzkConnect(t, handler, "", `{"code":"c"}`); response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

// TestChzzkConnectEndpointStoresAccount: 연결 후 기존 조회 엔드포인트에
// 치지직 채널명이 보인다 — 새 조회 API 없이 성립해야 한다.
func TestChzzkConnectEndpointStoresAccount(t *testing.T) {
	stub := newChzzkStub(t)
	store := newMemoryStreamingAccountStore()
	tokens, handler, _ := testChzzkHandler(t, stub, store)
	pair, err := tokens.IssuePair(context.Background(), uuid.New(), ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}

	response := postChzzkConnect(t, handler, pair.AccessToken, `{"code":"auth-code","state":"s"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	payload := map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["provider"] != string(StreamingProviderChzzk) || payload["connected"] != true {
		t.Fatalf("payload = %v", payload)
	}

	listed := listStreamingAccounts(t, handler, pair.AccessToken)
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listed.Code, listed.Body.String())
	}
	if !strings.Contains(listed.Body.String(), "테스트 채널") {
		t.Fatalf("connected channel is not listed: %s", listed.Body.String())
	}
}

// TestChzzkConnectRejectsInvalidCodeOverHTTP: 무효 code는 400 invalid_auth_code다.
func TestChzzkConnectRejectsInvalidCodeOverHTTP(t *testing.T) {
	stub := newChzzkStub(t)
	stub.rejectExchange = true
	tokens, handler, _ := testChzzkHandler(t, stub, newMemoryStreamingAccountStore())
	pair, err := tokens.IssuePair(context.Background(), uuid.New(), ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}

	response := postChzzkConnect(t, handler, pair.AccessToken, `{"code":"bad","state":"s"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "invalid_auth_code") {
		t.Fatalf("body = %s, want invalid_auth_code", response.Body.String())
	}
}

// TestChzzkDisconnectUsesExistingEndpoint: 연결 해제는 새 엔드포인트 없이
// 기존 DELETE /auth/streaming/accounts/{provider}로 동작하고, revoke가 나가며
// 행이 사라진다(#228 통과 기준).
func TestChzzkDisconnectUsesExistingEndpoint(t *testing.T) {
	stub := newChzzkStub(t)
	store := newMemoryStreamingAccountStore()
	tokens, handler, _ := testChzzkHandler(t, stub, store)
	userID := uuid.New()
	pair, err := tokens.IssuePair(context.Background(), userID, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if response := postChzzkConnect(t, handler, pair.AccessToken, `{"code":"auth-code","state":"s"}`); response.Code != http.StatusOK {
		t.Fatalf("connect status = %d, body = %s", response.Code, response.Body.String())
	}

	request := httptest.NewRequest(http.MethodDelete, "/auth/streaming/accounts/chzzk", nil)
	request.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent && response.Code != http.StatusOK {
		t.Fatalf("disconnect status = %d, body = %s", response.Code, response.Body.String())
	}
	if stub.revokes != 1 {
		t.Fatalf("revoke calls = %d, want the disconnect hook to revoke once", stub.revokes)
	}
	if _, err := store.Get(context.Background(), userID, StreamingProviderChzzk); err == nil {
		t.Fatal("the account row must be removed on disconnect")
	}
}

// TestChzzkConfigOmitsSecret: config는 공개 값만 준다.
func TestChzzkConfigOmitsSecret(t *testing.T) {
	_, handler, _ := testChzzkHandler(t, newChzzkStub(t), newMemoryStreamingAccountStore())
	request := httptest.NewRequest(http.MethodGet, "/auth/chzzk/config?state=abc", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if strings.Contains(body, "secret") {
		t.Fatalf("config must not expose the client secret: %s", body)
	}
	for _, want := range []string{"client_id", "redirect_uri", "authorize_url", ChzzkScopeStreamKeyRead} {
		if !strings.Contains(body, want) {
			t.Fatalf("config body %s is missing %q", body, want)
		}
	}
}
