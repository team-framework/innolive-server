package usage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestHeartbeatStoreSavesRow: 판정 컬럼과 원문이 함께 저장된다. 실사용 세션
// 행이 없어도(비동기 기록이 늦은 경우) 거절되지 않는다.
func TestHeartbeatStoreSavesRow(t *testing.T) {
	db := newPostgresUsageTestDB(t)
	store := NewHeartbeatStore(db)
	sessionID := uuid.New()
	sentAt := time.Date(2026, 10, 12, 8, 30, 0, 0, time.UTC)
	payload := `{"sent_at":"2026-10-12T08:30:00Z","device":{"thermal":"fair"}}`

	if err := store.SaveHeartbeat(context.Background(), Heartbeat{
		SessionID: sessionID, ReceivedAt: sentAt.Add(time.Second), SentAt: sentAt,
		ConnectedTargets: 2, VideoFPS: 29.8, DroppedFrames: 12, FailedFrames: 1, Platform: "ios", Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	var saved Heartbeat
	if err := db.Where("session_id = ?", sessionID).First(&saved).Error; err != nil {
		t.Fatal(err)
	}
	if saved.ID == uuid.Nil || saved.ConnectedTargets != 2 || saved.FailedFrames != 1 || saved.VideoFPS != 29.8 {
		t.Fatalf("saved = %+v", saved)
	}
	var thermal string
	if err := db.Raw("SELECT payload->'device'->>'thermal' FROM usage_heartbeats WHERE session_id = ?", sessionID).Scan(&thermal).Error; err != nil {
		t.Fatal(err)
	}
	if thermal != "fair" {
		t.Fatalf("payload thermal = %q, want the raw payload queryable as jsonb", thermal)
	}
}
