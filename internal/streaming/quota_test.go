package streaming

import (
	"net/http"
	"testing"
	"time"
)

// 읽기 1·쓰기 50으로 세고, 태평양 시간 자정에 0으로 돌아간다(#361).
func TestQuotaMeterCountsAndResetsAtPacificMidnight(t *testing.T) {
	clock := time.Date(2026, 9, 30, 6, 59, 0, 0, time.UTC) // PDT 9/29 23:59
	meter := NewQuotaMeter()
	meter.now = func() time.Time { return clock }
	meter.Record(http.MethodGet)
	meter.Record(http.MethodPost)
	meter.Record(http.MethodDelete)
	if used := meter.Used(); used != 101 {
		t.Fatalf("used = %d, want 101", used)
	}
	clock = clock.Add(2 * time.Minute) // PDT 9/30 00:01
	if used := meter.Used(); used != 0 {
		t.Fatalf("used after reset = %d, want 0", used)
	}
}

func TestQuotaMeterLowAtEightyPercent(t *testing.T) {
	meter := NewQuotaMeter()
	for i := 0; i < 159; i++ {
		meter.Record(http.MethodPost)
	}
	if meter.Low() {
		t.Fatal("7,950 units must not be low")
	}
	meter.Record(http.MethodPost)
	if !meter.Low() {
		t.Fatal("8,000 units must be low")
	}
}
