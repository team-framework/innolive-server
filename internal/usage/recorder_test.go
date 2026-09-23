package usage

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// TestRecordUsageDoesNotBlockWhenBufferFull: DB가 멈춰 버퍼가 차도 세션
// 경로는 블록되지 않고 사건을 버린다.
func TestRecordUsageDoesNotBlockWhenBufferFull(t *testing.T) {
	recorder := &Recorder{logger: discardLogger, events: make(chan session.UsageEvent, 1)}
	done := make(chan struct{})
	go func() {
		recorder.RecordUsage(session.UsageEvent{Kind: session.UsageSessionStarted})
		recorder.RecordUsage(session.UsageEvent{Kind: session.UsageSessionStarted})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RecordUsage blocked on a full buffer")
	}
}

// TestPostgresRecorderLifecycle: 사건이 두 테이블에 이어져 쓰이고, 일시정지
// 시간이 누적되며, Close가 남은 사건까지 쓴다.
func TestPostgresRecorderLifecycle(t *testing.T) {
	db := newPostgresUsageTestDB(t)
	user := createTestUser(t, db)
	recorder := NewRecorder(db, discardLogger)

	sessionID, broadcastID := uuid.NewString(), uuid.NewString()
	start := time.Now().UTC().Truncate(time.Microsecond)
	for _, event := range []session.UsageEvent{
		{Kind: session.UsageSessionStarted, At: start, SessionID: sessionID, UserID: user, AIProcessing: session.AIProcessingServer},
		{Kind: session.UsageBroadcastStarted, At: start.Add(10 * time.Second), SessionID: sessionID, BroadcastID: broadcastID, Provider: "youtube"},
		{Kind: session.UsageBroadcastLive, At: start.Add(20 * time.Second), BroadcastID: broadcastID},
		{Kind: session.UsageBroadcastPaused, At: start.Add(30 * time.Second), BroadcastID: broadcastID},
		{Kind: session.UsageBroadcastResumed, At: start.Add(40 * time.Second), BroadcastID: broadcastID},
		{Kind: session.UsageBroadcastPaused, At: start.Add(50 * time.Second), BroadcastID: broadcastID},
		{Kind: session.UsageBroadcastEnded, At: start.Add(55 * time.Second), BroadcastID: broadcastID, Reason: "user_requested"},
		{Kind: session.UsageSessionEnded, At: start.Add(60 * time.Second), SessionID: sessionID, Reason: "peer_connection_closed"},
	} {
		recorder.RecordUsage(event)
	}
	if err := recorder.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	var gotSession Session
	if err := db.First(&gotSession, "session_id = ?", sessionID).Error; err != nil {
		t.Fatal(err)
	}
	if gotSession.UserID == nil || *gotSession.UserID != user || gotSession.IsGuest ||
		gotSession.EndedAt == nil || !gotSession.EndedAt.Equal(start.Add(60*time.Second)) ||
		*gotSession.EndReason != "peer_connection_closed" || *gotSession.AIProcessing != "server" {
		t.Fatalf("session row = %+v", gotSession)
	}
	var gotBroadcast Broadcast
	if err := db.First(&gotBroadcast, "id = ?", broadcastID).Error; err != nil {
		t.Fatal(err)
	}
	if gotBroadcast.LiveAt == nil || !gotBroadcast.LiveAt.Equal(start.Add(20*time.Second)) ||
		gotBroadcast.EndedAt == nil || *gotBroadcast.EndReason != "user_requested" ||
		gotBroadcast.PausedSeconds != 15 || gotBroadcast.Source != SourceLive {
		t.Fatalf("broadcast row = %+v", gotBroadcast)
	}
}

// TestPostgresRecorderGuestAndWithdrawal: 게스트는 user_id 없이 남고, 회원이
// 탈퇴하면 기록은 남기되 user_id만 지워진다.
func TestPostgresRecorderGuestAndWithdrawal(t *testing.T) {
	db := newPostgresUsageTestDB(t)
	user := createTestUser(t, db)
	recorder := NewRecorder(db, discardLogger)
	guestSession, memberSession := uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	recorder.RecordUsage(session.UsageEvent{Kind: session.UsageSessionStarted, At: now, SessionID: guestSession})
	recorder.RecordUsage(session.UsageEvent{Kind: session.UsageSessionStarted, At: now, SessionID: memberSession, UserID: user})
	if err := recorder.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	var guest Session
	if err := db.First(&guest, "session_id = ?", guestSession).Error; err != nil {
		t.Fatal(err)
	}
	if !guest.IsGuest || guest.UserID != nil {
		t.Fatalf("guest row = %+v", guest)
	}
	if err := db.Where("id = ?", user).Delete(&auth.User{}).Error; err != nil {
		t.Fatal(err)
	}
	var member Session
	if err := db.First(&member, "session_id = ?", memberSession).Error; err != nil {
		t.Fatalf("withdrawal removed usage row: %v", err)
	}
	if member.UserID != nil || member.IsGuest {
		t.Fatalf("withdrawn member row = %+v, want user_id NULL and is_guest false", member)
	}
}

// TestPostgresRecorderSurvivesWriteFailure: 쓰기가 실패해도 워커는 멈추지 않고
// 다음 사건을 쓴다.
func TestPostgresRecorderSurvivesWriteFailure(t *testing.T) {
	db := newPostgresUsageTestDB(t)
	recorder := NewRecorder(db, discardLogger)
	// 존재하지 않는 세션의 송출은 FK 위반으로 실패한다.
	recorder.RecordUsage(session.UsageEvent{Kind: session.UsageBroadcastStarted, At: time.Now().UTC(),
		SessionID: uuid.NewString(), BroadcastID: uuid.NewString(), Provider: "youtube"})
	next := uuid.NewString()
	recorder.RecordUsage(session.UsageEvent{Kind: session.UsageSessionStarted, At: time.Now().UTC(), SessionID: next})
	if err := recorder.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&Broadcast{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("broadcast count = %d, err = %v", count, err)
	}
	if err := db.First(&Session{}, "session_id = ?", next).Error; err != nil {
		t.Fatalf("event after failure was not written: %v", err)
	}
}

// TestPostgresCloseOrphans: 종료 기록 없이 남은 live 행만 unclean_shutdown으로
// 마감하고, 백필 행과 정상 종료 행은 건드리지 않는다.
func TestPostgresCloseOrphans(t *testing.T) {
	db := newPostgresUsageTestDB(t)
	now := time.Now().UTC()
	ended, reason := now, "user_requested"
	rows := []Session{
		{ID: uuid.New(), IsGuest: true, StartedAt: now, Source: SourceLive},
		{ID: uuid.New(), IsGuest: true, StartedAt: now, Source: SourceBackfill},
		{ID: uuid.New(), IsGuest: true, StartedAt: now, EndedAt: &ended, EndReason: &reason, Source: SourceLive},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	orphanBroadcast := Broadcast{ID: uuid.New(), SessionID: rows[0].ID, Provider: "chzzk", StartedAt: now, Source: SourceLive}
	if err := db.Create(&orphanBroadcast).Error; err != nil {
		t.Fatal(err)
	}
	if err := CloseOrphans(context.Background(), db); err != nil {
		t.Fatal(err)
	}

	want := []*string{ptr(ReasonUncleanShutdown), nil, ptr("user_requested")}
	for index, row := range rows {
		var got Session
		if err := db.First(&got, "session_id = ?", row.ID).Error; err != nil {
			t.Fatal(err)
		}
		if (got.EndReason == nil) != (want[index] == nil) || (got.EndReason != nil && *got.EndReason != *want[index]) {
			t.Fatalf("row %d end_reason = %v, want %v", index, got.EndReason, want[index])
		}
	}
	var broadcast Broadcast
	if err := db.First(&broadcast, "id = ?", orphanBroadcast.ID).Error; err != nil {
		t.Fatal(err)
	}
	if broadcast.EndReason == nil || *broadcast.EndReason != ReasonUncleanShutdown || broadcast.EndedAt != nil {
		t.Fatalf("orphan broadcast = %+v", broadcast)
	}
}

func ptr(value string) *string { return &value }

func createTestUser(t *testing.T, db *gorm.DB) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	user := auth.User{ID: uuid.New(), Status: auth.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	return user.ID
}

func newPostgresUsageTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL usage integration test")
	}
	admin, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	schema := "usage_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Errorf("drop PostgreSQL test schema: %v", err)
		}
	})

	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.AutoMigrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := AutoMigrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}
