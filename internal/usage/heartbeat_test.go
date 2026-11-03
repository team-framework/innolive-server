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

// TestHeartbeatStoreCompactKeepsLatestPerSession: 보존 기간이 지난 하트비트는 세션마다
// 가장 늦은 한 건만 남고, 기간 안의 행은 그대로다. 원장이 읽는 세션별 마지막 시각은
// 압축 전후로 같다(P1-33).
func TestHeartbeatStoreCompactKeepsLatestPerSession(t *testing.T) {
	db := newPostgresUsageTestDB(t)
	store := NewHeartbeatStore(db)
	ctx := context.Background()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	mixed, oldOnly, single, recent := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	rows := map[uuid.UUID][]time.Duration{
		mixed:   {40 * day, 35 * day, day},      // 오래된 두 건은 지우고 최근 한 건을 남긴다
		oldOnly: {45 * day, 40 * day, 31 * day}, // 가장 늦은 31일 전 행만 남긴다
		single:  {60 * day},                     // 하나뿐이면 그대로다
		recent:  {10 * day, 5 * day, time.Hour}, // 보존 기간 안이면 모두 남긴다
	}
	for sessionID, ages := range rows {
		for _, age := range ages {
			at := now.Add(-age)
			if err := store.SaveHeartbeat(ctx, Heartbeat{SessionID: sessionID, ReceivedAt: at, SentAt: at, Platform: "ios", Payload: "{}"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	lastBefore := lastHeartbeatTimes(t, store, mixed, oldOnly, single, recent)

	deleted, err := store.Compact(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 4 {
		t.Fatalf("deleted = %d, want 4", deleted)
	}
	wantCounts := map[uuid.UUID]int64{mixed: 1, oldOnly: 1, single: 1, recent: 3}
	for sessionID, want := range wantCounts {
		var count int64
		if err := db.Model(&Heartbeat{}).Where("session_id = ?", sessionID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("session %s rows = %d, want %d", sessionID, count, want)
		}
	}
	lastAfter := lastHeartbeatTimes(t, store, mixed, oldOnly, single, recent)
	for sessionID, before := range lastBefore {
		if !lastAfter[sessionID].Equal(before) {
			t.Fatalf("session %s last heartbeat = %v, want %v", sessionID, lastAfter[sessionID], before)
		}
	}
	if again, err := store.Compact(ctx, now); err != nil || again != 0 {
		t.Fatalf("second compact = %d, %v; want nothing left to delete", again, err)
	}
}

// TestHeartbeatStoreCompactReportsDatabaseError: 끝난 컨텍스트로 부르면 오류를 돌려주고
// 아무것도 지우지 않는다.
func TestHeartbeatStoreCompactReportsDatabaseError(t *testing.T) {
	db := newPostgresUsageTestDB(t)
	store := NewHeartbeatStore(db)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	sessionID := uuid.New()
	for _, age := range []time.Duration{50 * 24 * time.Hour, 40 * 24 * time.Hour} {
		at := now.Add(-age)
		if err := store.SaveHeartbeat(context.Background(), Heartbeat{SessionID: sessionID, ReceivedAt: at, SentAt: at, Platform: "ios", Payload: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if deleted, err := store.Compact(ctx, now); err == nil || deleted != 0 {
		t.Fatalf("Compact with canceled context = %d, %v; want an error", deleted, err)
	}
	var count int64
	if err := db.Model(&Heartbeat{}).Where("session_id = ?", sessionID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("rows = %d, want 2 (nothing deleted)", count)
	}
}

func lastHeartbeatTimes(t *testing.T, store *HeartbeatStore, sessionIDs ...uuid.UUID) map[uuid.UUID]time.Time {
	t.Helper()
	last, err := NewLedger(store.db).lastHeartbeats(context.Background(), sessionIDs)
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != len(sessionIDs) {
		t.Fatalf("last heartbeats = %d sessions, want %d", len(last), len(sessionIDs))
	}
	return last
}
