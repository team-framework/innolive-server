package analytics_test

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"inno-live-server/internal/analytics"
	"inno-live-server/internal/config"
	"inno-live-server/internal/database/migration"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL for PostgreSQL integration")
	}
	admin, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlAdmin, _ := admin.DB()
	t.Cleanup(func() { sqlAdmin.Close() })
	schema := "analytics_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	return db
}

func TestPostgresGenericPropertiesDedupRestartRetention(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := analytics.AutoMigrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	store := analytics.NewStore(db)
	e := analytics.Event{Version: 1, EventID: uuid.NewString(), VisitID: uuid.NewString(), Sequence: 1, Event: "future_event", Properties: map[string]any{"arbitrary_metric": 12.5, "enabled": true, "variant": "a"}, Release: "test"}
	if err := store.Save(ctx, e); err != nil {
		t.Fatal(err)
	}
	var original analytics.Row
	db.First(&original)
	e.Properties["arbitrary_metric"] = 99.0
	if err := analytics.NewStore(db).Save(ctx, e); err != nil {
		t.Fatal(err)
	}
	var row analytics.Row
	db.First(&row)
	var properties map[string]any
	if err := json.Unmarshal([]byte(row.Properties), &properties); err != nil {
		t.Fatal(err)
	}
	if properties["arbitrary_metric"] != 12.5 || !row.ReceivedAt.Equal(original.ReceivedAt) {
		t.Fatal("duplicate rewrote stored event")
	}
	old := time.Now().Add(-analytics.Retention - time.Second)
	db.Model(&analytics.Row{}).Where("event_id = ?", e.EventID).Update("received_at", old)
	e.EventID = uuid.NewString()
	e.Sequence = 2
	if err := store.Save(ctx, e); err != nil {
		t.Fatal(err)
	}
	n, err := store.Purge(ctx, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("purge %d %v", n, err)
	}
	var count int64
	db.Model(&analytics.Row{}).Count(&count)
	if count != 1 {
		t.Fatal("recent event lost")
	}
}

func TestVersionedMigrationAndOrderedFunnel(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	var schema string
	db.Raw("SELECT current_schema()").Scan(&schema)
	parsed, _ := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	for i := 0; i < 2; i++ {
		if err := migration.Run(ctx, db, parsed.String(), config.DatabaseMigrationModeVersioned); err != nil {
			t.Fatal(err)
		}
	}
	store := analytics.NewStore(db)
	visitA, visitB, visitC := uuid.NewString(), uuid.NewString(), uuid.NewString()
	add := func(visit, name string, seq int64) {
		t.Helper()
		err := store.Save(ctx, analytics.Event{Version: 1, EventID: uuid.NewString(), VisitID: visit, Sequence: seq, Event: name, Properties: map[string]any{"locale": "ko"}, Release: "test"})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Arrival order is inverted but the client sequence preserves the real flow.
	add(visitA, "signup_completed", 6)
	add(visitA, "experience_succeeded", 3)
	add(visitA, "experience_started", 2)
	add(visitA, "landing_viewed", 1)
	add(visitA, "signup_viewed", 4)
	add(visitA, "signup_verification_sent", 5)
	add(visitB, "signup_completed", 1)
	add(visitB, "landing_viewed", 2)
	add(visitB, "experience_started", 3)
	add(visitB, "experience_succeeded", 4)
	add(visitC, "experience_succeeded", 1)
	add(visitC, "landing_viewed", 2)
	add(visitC, "experience_started", 3)
	sql, err := os.ReadFile("../../scripts/analytics/funnel.sql")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		Funnel string
		Step   int
		Visits int64
	}
	var rows []result
	if err := db.Raw(string(sql)).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	want := []int64{3, 1, 1, 1, 3, 3, 2, 1}
	if len(rows) != len(want) {
		t.Fatal("missing funnel steps")
	}
	for i, r := range rows {
		if r.Visits != want[i] {
			t.Fatalf("%s step %d got %d want %d", r.Funnel, r.Step, r.Visits, want[i])
		}
	}
	db.Exec("TRUNCATE analytics_events")
	rows = nil
	if err := db.Raw(string(sql)).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 8 {
		t.Fatal("empty cohort dropped stages")
	}
	for _, r := range rows {
		if r.Visits != 0 {
			t.Fatal("nonzero empty cohort")
		}
	}
}
