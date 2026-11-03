package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 클라이언트가 보낸 X-Request-ID를 그대로 응답·로그에 실으면 길이 제한 없이 로그를
// 부풀리고 줄바꿈 같은 제어 문자로 로그를 위조할 수 있다(P2-9).
func TestRequestIDMiddlewareReplacesUnsafeIDs(t *testing.T) {
	handler := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for name, id := range map[string]string{
		"too long":      strings.Repeat("a", 200),
		"control chars": "abc\x01def",
		"non ascii":     "요청-1",
	} {
		request := httptest.NewRequest(http.MethodGet, "/health", nil)
		request.Header.Set("X-Request-ID", id)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if got := response.Header().Get("X-Request-ID"); got == id || got == "" || len(got) > 128 {
			t.Errorf("%s: response X-Request-ID = %q, want a fresh safe ID", name, got)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	request.Header.Set("X-Request-ID", "client-req_01.A")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if got := response.Header().Get("X-Request-ID"); got != "client-req_01.A" {
		t.Fatalf("safe client ID was replaced with %q", got)
	}
}
