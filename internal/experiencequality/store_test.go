package experiencequality_test

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"inno-live-server/internal/config"
	"inno-live-server/internal/database/migration"
	"inno-live-server/internal/experiencequality"
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
	schema := "quality_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

func TestPostgresDuplicateRetentionAndHTTP(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := experiencequality.AutoMigrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := experiencequality.AutoMigrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := experiencequality.NewStore(db)
	body := `{"version":1,"attemptId":"12345678-1234-4234-8234-123456789abc","event":"started","role":"guest","locale":"ko","retry":false,"stage":"session","elapsedMs":0}`
	e, err := experiencequality.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	h := experiencequality.NewHandler(store, "test-service-key-at-least-32-characters")
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/experience-quality", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer test-service-key-at-least-32-characters")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 204 {
			t.Fatalf("HTTP persistence: %d", w.Code)
		}
	}
	var count int64
	db.Model(&experiencequality.Row{}).Count(&count)
	if count != 1 {
		t.Fatalf("duplicate rows: %d", count)
	}
	now := time.Now().UTC()
	old := now.Add(-experiencequality.Retention - time.Second)
	if err := db.Model(&experiencequality.Row{}).Where("attempt_id = ?", e.AttemptID).Update("received_at", old).Error; err != nil {
		t.Fatal(err)
	}
	e.Role = "member"
	if err := store.Save(ctx, e); err != nil {
		t.Fatal(err)
	}
	var row experiencequality.Row
	db.First(&row)
	if row.Role != "guest" || row.ReceivedAt.Sub(old).Abs() > time.Microsecond {
		t.Fatal("duplicate rewrote original row or extended retention")
	}
	e.AttemptID = uuid.NewString()
	if err := store.Save(ctx, e); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.Purge(ctx, now)
	if err != nil || deleted != 1 {
		t.Fatalf("purge: %d %v", deleted, err)
	}
	db.Model(&experiencequality.Row{}).Count(&count)
	if count != 1 {
		t.Fatalf("recent event removed: %d", count)
	}
	// A new store instance uses the same persisted data after a process restart.
	restarted := experiencequality.NewStore(db)
	if err := restarted.Save(ctx, e); err != nil {
		t.Fatal(err)
	}
	db.Model(&experiencequality.Row{}).Count(&count)
	if count != 1 {
		t.Fatal("restart lost deduplication")
	}
}

func TestVersionedMigrationCreatesCollector(t *testing.T) {
	db := testDB(t)
	var schema string
	if err := db.Raw("SELECT current_schema()").Scan(&schema).Error; err != nil || !strings.HasPrefix(schema, "quality_test_") {
		t.Fatalf("test schema unavailable: %v", err)
	}
	parsed, _ := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	if err := migration.Run(context.Background(), db, parsed.String(), config.DatabaseMigrationModeVersioned); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := db.Raw("SELECT to_regclass('experience_quality_events') IS NOT NULL").Scan(&exists).Error; err != nil || !exists {
		t.Fatalf("versioned table: %v %v", exists, err)
	}
	if err := migration.Run(context.Background(), db, parsed.String(), config.DatabaseMigrationModeVersioned); err != nil {
		t.Fatal(err)
	}
}
