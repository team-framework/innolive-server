package clientip

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolverUsesForwardedChainOnlyFromTrustedPeer(t *testing.T) {
	resolver := NewResolver([]string{"10.0.0.0/8"})
	cases := []struct {
		name, remote, forwarded, want string
	}{
		{"trusted peer picks nearest untrusted hop", "10.0.0.10:443", "198.51.100.99, 203.0.113.25", "203.0.113.25"},
		{"trusted peer skips trusted hops", "10.0.0.10:443", "203.0.113.25, 10.0.0.20", "203.0.113.25"},
		{"trusted peer without header", "10.0.0.10:443", "", "10.0.0.10"},
		{"untrusted peer ignores header", "203.0.113.25:443", "198.51.100.99", "203.0.113.25"},
	}
	for _, tc := range cases {
		if got := resolver.IP(tc.remote, tc.forwarded); got != tc.want {
			t.Errorf("%s: IP = %q, want %q", tc.name, got, tc.want)
		}
	}
	if !resolver.RemoteTrusted("10.1.2.3:80") || resolver.RemoteTrusted("203.0.113.25:80") {
		t.Error("RemoteTrusted does not follow the trusted range")
	}
}

func TestMiddlewareCarriesResolvedIPToHandlers(t *testing.T) {
	var got string
	handler := NewResolver([]string{"172.16.0.0/12"}).Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = FromRequest(r)
	}))
	request := httptest.NewRequest(http.MethodPost, "/auth/sign-in", nil)
	request.RemoteAddr = "172.17.0.1:51000"
	request.Header.Set("X-Forwarded-For", "203.0.113.7")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if got != "203.0.113.7" {
		t.Fatalf("client IP behind trusted proxy = %q, want forwarded client", got)
	}

	request = httptest.NewRequest(http.MethodPost, "/auth/sign-in", nil)
	request.RemoteAddr = "198.51.100.4:51000"
	request.Header.Set("X-Forwarded-For", "203.0.113.7")
	if FromRequest(request) != "198.51.100.4" {
		t.Fatal("request outside the middleware must fall back to the peer address")
	}
}

func TestRateLimitIPIsEmptyBehindUntrustedProxy(t *testing.T) {
	var limitIP string
	handler := NewResolver(nil).Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		limitIP = RateLimitIP(r)
	}))
	request := httptest.NewRequest(http.MethodPost, "/auth/sign-in", nil)
	request.RemoteAddr = "172.17.0.1:51000"
	request.Header.Set("X-Forwarded-For", "203.0.113.7")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if limitIP != "" {
		t.Fatalf("rate-limit IP behind an untrusted proxy = %q, want empty", limitIP)
	}

	request = httptest.NewRequest(http.MethodPost, "/auth/sign-in", nil)
	request.RemoteAddr = "198.51.100.4:51000"
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if limitIP != "198.51.100.4" {
		t.Fatalf("rate-limit IP for a direct client = %q", limitIP)
	}
}
