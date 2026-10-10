package session

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"inno-live-server/internal/media"
)

// TestLogMosaicSummaryWritesSessionTotals는 세션 종료 요약 로그만으로 방송 1회의
// 얼굴·번호판 블러 프레임 수를 읽을 수 있는지 지킨다(#412).
func TestLogMosaicSummaryWritesSessionTotals(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))

	logMosaicSummary(logger, "session-1", media.NewMosaicTally())

	var entry map[string]any
	if err := json.Unmarshal(buffer.Bytes(), &entry); err != nil {
		t.Fatalf("summary log is not one JSON line: %v (%q)", err, buffer.String())
	}
	if entry["msg"] != "session mosaic summary" || entry["session_id"] != "session-1" {
		t.Fatalf("summary log = %v", entry)
	}
	for _, key := range []string{
		"frames_face_only", "frames_plate_only", "frames_both", "frames_none",
		"face_objects", "number_plate_objects", "whitelisted_face_objects",
	} {
		if value, ok := entry[key]; !ok || value != float64(0) {
			t.Errorf("%s = %v, want 0", key, value)
		}
	}
}

func TestLogMosaicSummarySkipsMissingTally(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))

	logMosaicSummary(logger, "session-1", nil)

	if buffer.Len() != 0 {
		t.Fatalf("summary log without tally = %q, want nothing", buffer.String())
	}
}
