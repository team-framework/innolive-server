package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestRecordPlatformTimes: 송출 세대에 플랫폼 실제 시각이 남아 서버 기록(LiveAt·
// EndedAt)과 대조할 수 있다. 행이 아직 없으면 알린다(#437).
func TestRecordPlatformTimes(t *testing.T) {
	db := newPostgresUsageTestDB(t)
	store := NewPlatformTimesStore(db)
	ctx := context.Background()
	sessionID, broadcastID := uuid.New(), uuid.New()
	liveAt := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	endedAt := liveAt.Add(30 * time.Minute)
	if err := db.Create(&Session{ID: sessionID, StartedAt: liveAt, Source: SourceLive}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Broadcast{ID: broadcastID, SessionID: sessionID, Provider: "youtube", StartedAt: liveAt, LiveAt: &liveAt, EndedAt: &endedAt, Source: SourceLive}).Error; err != nil {
		t.Fatal(err)
	}

	platformStart, platformEnd := liveAt.Add(5*time.Second), endedAt.Add(10*time.Second)
	if err := store.RecordPlatformTimes(ctx, broadcastID.String(), "yt-1", &platformStart, &platformEnd); err != nil {
		t.Fatal(err)
	}
	var startGap, endGap float64
	if err := db.Raw(`SELECT EXTRACT(EPOCH FROM platform_started_at - live_at), EXTRACT(EPOCH FROM platform_ended_at - ended_at)
		FROM usage_broadcasts WHERE id = ? AND platform_broadcast_id = 'yt-1'`, broadcastID).Row().Scan(&startGap, &endGap); err != nil {
		t.Fatal(err)
	}
	if startGap != 5 || endGap != 10 {
		t.Fatalf("gaps = %v / %v, want 5 / 10 seconds", startGap, endGap)
	}
	if err := store.RecordPlatformTimes(ctx, uuid.NewString(), "yt-2", &platformStart, nil); !errors.Is(err, errBroadcastNotRecorded) {
		t.Fatalf("missing row = %v, want errBroadcastNotRecorded", err)
	}
}
