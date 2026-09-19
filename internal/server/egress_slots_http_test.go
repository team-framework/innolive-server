package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"inno-live-server/internal/media"
)

// TestEgressSlotsExhaustedMapsTo503: 자리는 다른 방송이 끝나야 나므로 409(충돌)나
// 500(서버 결함)이 아니라 503이어야 한다 — 클라이언트가 "나중에 다시"로 안내한다.
func TestEgressSlotsExhaustedMapsTo503(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Server{}).writeStartStreamError(recorder, media.ErrEgressSlotsExhausted, "session-1")

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "egress_slots_exhausted") {
		t.Fatalf("body = %s, want the egress_slots_exhausted code", body)
	}
}
