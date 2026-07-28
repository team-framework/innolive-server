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
	"unicode/utf8"

	"github.com/google/uuid"
)

func testTokenHTTPHandler(t *testing.T, config TokenHTTPConfig) (*TokenService, http.Handler) {
	t.Helper()
	service := testTokenService(newMemoryRefreshStore())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	return service, MountTokenHTTP(next, service, logger, config)
}

func TestTokenHTTPResponseDisablesCaching(t *testing.T) {
	config, err := NewTokenHTTPConfig(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, handler := testTokenHTTPHandler(t, config)
	request := httptest.NewRequest(http.MethodPost, "/auth/refresh", strings.NewReader(`{"refresh_token":"invalid"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := response.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q", got)
	}
}

func TestTokenHTTPCORSAllowlist(t *testing.T) {
	config, err := NewTokenHTTPConfig(false, []string{"http://localhost:5173"})
	if err != nil {
		t.Fatal(err)
	}
	_, handler := testTokenHTTPHandler(t, config)

	allowed := httptest.NewRequest(http.MethodOptions, "/auth/refresh", nil)
	allowed.Header.Set("Origin", "http://localhost:5173")
	allowedResponse := httptest.NewRecorder()
	handler.ServeHTTP(allowedResponse, allowed)
	if allowedResponse.Code != http.StatusNoContent {
		t.Fatalf("allowed status = %d", allowedResponse.Code)
	}
	if got := allowedResponse.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("allowed origin = %q", got)
	}

	disallowed := httptest.NewRequest(http.MethodOptions, "/auth/refresh", nil)
	disallowed.Header.Set("Origin", "https://evil.example")
	disallowedResponse := httptest.NewRecorder()
	handler.ServeHTTP(disallowedResponse, disallowed)
	if disallowedResponse.Code != http.StatusForbidden {
		t.Fatalf("disallowed status = %d, want %d", disallowedResponse.Code, http.StatusForbidden)
	}
}

func TestTokenHTTPCORSAllowAllUsesWildcardWithoutCredentials(t *testing.T) {
	config, err := NewTokenHTTPConfig(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, handler := testTokenHTTPHandler(t, config)
	request := httptest.NewRequest(http.MethodOptions, "/auth/refresh", nil)
	request.Header.Set("Origin", "https://any.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("allow origin = %q", got)
	}
	if got := response.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("credentials header must be absent, got %q", got)
	}
}

func TestTokenHTTPRefreshRotatesRealPair(t *testing.T) {
	config, err := NewTokenHTTPConfig(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	service, handler := testTokenHTTPHandler(t, config)
	first, err := service.IssuePair(context.Background(), uuid.New(), ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"refresh_token": first.RefreshToken})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/auth/refresh", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var second TokenPair
	if err := json.NewDecoder(response.Body).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if second.RefreshToken == "" || second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh token was not rotated")
	}
	if _, err := service.ValidateAccessToken(second.AccessToken); err != nil {
		t.Fatalf("new access token invalid: %v", err)
	}
}

func TestTruncateTokenMetadataPreservesUTF8(t *testing.T) {
	value := strings.Repeat("가😀", 200) + string([]byte{0xff})
	truncated := truncateTokenMetadata(value, 512)

	if len(truncated) > 512 {
		t.Fatalf("length = %d, want at most 512", len(truncated))
	}
	if !utf8.ValidString(truncated) {
		t.Fatalf("truncated metadata is invalid UTF-8: %q", truncated)
	}
}
