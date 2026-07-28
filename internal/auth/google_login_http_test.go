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

func testGoogleLoginHTTPHandler(t *testing.T, verifier GoogleIdentityVerifier, user googleLoginUser) http.Handler {
	t.Helper()
	config, err := NewTokenHTTPConfig(false, []string{"http://localhost:3000"})
	if err != nil {
		t.Fatal(err)
	}
	tokens := testTokenService(newMemoryRefreshStore())
	google, err := NewGoogleLoginService(verifier, &stubGoogleAccounts{user: user}, tokens)
	if err != nil {
		t.Fatal(err)
	}
	return MountAuthHTTP(http.NotFoundHandler(), tokens, google, slog.New(slog.NewTextHandler(io.Discard, nil)), config)
}

func TestGoogleLoginHTTPIssuesTokenPairAndUsesCORS(t *testing.T) {
	verifier := &stubGoogleVerifier{identity: GoogleIdentity{Subject: "google-subject", Email: "person@example.com", EmailVerified: true}}
	handler := testGoogleLoginHTTPHandler(t, verifier, googleLoginUser{ID: uuid.New(), Status: UserStatusActive})

	request := httptest.NewRequest(http.MethodPost, "/auth/google", bytes.NewBufferString(`{"id_token":"google-id-token"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:3000")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("allow origin = %q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache control = %q", got)
	}
	var pair TokenPair
	if err := json.NewDecoder(response.Body).Decode(&pair); err != nil {
		t.Fatal(err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatalf("token pair = %+v", pair)
	}
}

func TestGoogleLoginHTTPRejectsInvalidTokenAndBadRequest(t *testing.T) {
	invalidHandler := testGoogleLoginHTTPHandler(
		t,
		&stubGoogleVerifier{err: ErrInvalidGoogleIDToken},
		googleLoginUser{ID: uuid.New(), Status: UserStatusActive},
	)
	invalidRequest := httptest.NewRequest(http.MethodPost, "/auth/google", bytes.NewBufferString(`{"id_token":"forged"}`))
	invalidResponse := httptest.NewRecorder()
	invalidHandler.ServeHTTP(invalidResponse, invalidRequest)
	if invalidResponse.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status = %d, body = %s", invalidResponse.Code, invalidResponse.Body.String())
	}

	badRequest := httptest.NewRequest(http.MethodPost, "/auth/google", bytes.NewBufferString(`{"id_token":"token","extra":true}`))
	badResponse := httptest.NewRecorder()
	invalidHandler.ServeHTTP(badResponse, badRequest)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("bad request status = %d, body = %s", badResponse.Code, badResponse.Body.String())
	}

	options := httptest.NewRequest(http.MethodOptions, "/auth/google", nil)
	options.Header.Set("Origin", "http://localhost:3000")
	optionsResponse := httptest.NewRecorder()
	invalidHandler.ServeHTTP(optionsResponse, options)
	if optionsResponse.Code != http.StatusNoContent {
		t.Fatalf("options status = %d", optionsResponse.Code)
	}
}
