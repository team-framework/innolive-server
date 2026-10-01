package auth

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func testAppleLoginHTTPHandler(t *testing.T, verifier AppleIdentityVerifier) http.Handler {
	t.Helper()
	config, err := NewTokenHTTPConfig(false, []string{"http://localhost:3000"})
	if err != nil {
		t.Fatal(err)
	}
	tokens := testTokenService(newMemoryRefreshStore())
	service, err := NewAppleLoginService(
		&stubAppleExchanger{response: AppleTokenResponse{IDToken: "apple-id-token", RefreshToken: "apple-refresh-token"}},
		verifier,
		&stubAppleAccounts{user: appleLoginUser{ID: uuid.New(), Status: UserStatusActive}},
		tokens,
		&ProviderTokenCipher{key: []byte("0123456789abcdef0123456789abcdef")},
	)
	if err != nil {
		t.Fatal(err)
	}
	return MountAuthHTTP(http.NotFoundHandler(), tokens, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), config, service)
}

func TestAppleLoginHTTPIssuesTokenPair(t *testing.T) {
	handler := testAppleLoginHTTPHandler(t, &stubAppleVerifier{identity: AppleIdentity{Subject: "apple-subject", Email: "person@example.com", EmailVerified: true}})
	request := httptest.NewRequest(http.MethodPost, "/auth/apple", bytes.NewBufferString(`{"authorization_code":"single-use-code","nonce":"nonce","given_name":"Ada","family_name":"Lovelace"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:3000")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response headers = %v", response.Header())
	}
	var pair TokenPair
	if err := json.NewDecoder(response.Body).Decode(&pair); err != nil || pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatalf("pair=%+v err=%v", pair, err)
	}
}

func TestAppleLoginHTTPRejectsInvalidInput(t *testing.T) {
	handler := testAppleLoginHTTPHandler(t, &stubAppleVerifier{err: ErrInvalidAppleIDToken})
	invalid := httptest.NewRequest(http.MethodPost, "/auth/apple", bytes.NewBufferString(`{"authorization_code":"code"}`))
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status = %d", invalidResponse.Code)
	}
	bad := httptest.NewRequest(http.MethodPost, "/auth/apple", bytes.NewBufferString(`{"authorization_code":"code","unexpected":true}`))
	badResponse := httptest.NewRecorder()
	handler.ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("bad request status = %d", badResponse.Code)
	}
}
