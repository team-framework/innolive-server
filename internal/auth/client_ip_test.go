package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"inno-live-server/internal/clientip"
)

// Caddy가 Docker 브리지 주소로 서버에 연결하므로, 인증 경로가 RemoteAddr만 보면
// 모든 사용자가 같은 IP 하나로 묶여 IP 기준 제한이 전역 제한이 된다(P1-17).
func TestRequestClientInfoUsesSharedClientIP(t *testing.T) {
	var info ClientInfo
	handler := clientip.NewResolver([]string{"172.16.0.0/12"}).Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		info = requestClientInfo(r)
	}))

	request := httptest.NewRequest(http.MethodPost, "/auth/sign-in", nil)
	request.RemoteAddr = "172.17.0.1:51000"
	request.Header.Set("X-Forwarded-For", "203.0.113.7")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if info.IPAddress != "203.0.113.7" {
		t.Fatalf("client IP behind trusted proxy = %q, want forwarded client", info.IPAddress)
	}

	request = httptest.NewRequest(http.MethodPost, "/auth/sign-in", nil)
	request.RemoteAddr = "198.51.100.4:51000"
	request.Header.Set("X-Forwarded-For", "203.0.113.7")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if info.IPAddress != "198.51.100.4" {
		t.Fatalf("client IP from untrusted peer = %q, want peer address", info.IPAddress)
	}
	if info.RateLimitIP != "" {
		t.Fatalf("rate-limit IP behind an untrusted proxy = %q, want empty", info.RateLimitIP)
	}
}
