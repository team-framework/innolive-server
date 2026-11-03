package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"inno-live-server/internal/session"
)

// TestWriteSessionErrorNotFoundBody: 세션을 찾지 못한 응답의 상태 코드와 본문은 공용
// 함수로 모은 뒤에도 그대로다(P2-7). 감싼 오류도 같은 응답이 된다.
func TestWriteSessionErrorNotFoundBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeSessionError(recorder, fmt.Errorf("lookup: %w", session.ErrNotFound), "s-1")

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	want := `{"error":{"code":"not_found","details":{"session_id":"s-1"},"message":"Session not found."}}` + "\n"
	if got := recorder.Body.String(); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
}

// TestWriteSessionErrorUnknownIsInternal: 세션 없음이 아닌 오류는 내부 사유를 드러내지
// 않고 500으로 답한다.
func TestWriteSessionErrorUnknownIsInternal(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeSessionError(recorder, errors.New("database exploded"), "s-1")

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	want := `{"error":{"code":"internal_error","message":"An unexpected server error occurred."}}` + "\n"
	if got := recorder.Body.String(); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
}
