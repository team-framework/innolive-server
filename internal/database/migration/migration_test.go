package migration

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"inno-live-server/internal/config"
	"inno-live-server/internal/vod"
)

func TestAutoMigrationRepeatedStartup(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for PostgreSQL integration")
	}
	open := func(dsn string) *gorm.DB {
		t.Helper()
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
		if err != nil {
			t.Fatal(err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sqlDB.Close() })
		return db
	}
	admin := open(dsn)
	schema := "migration_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db := open(u.String())
	filename := "original.mp4"
	now := time.Now().UTC()
	job := vod.Job{ID: uuid.New(), UserID: uuid.New(), Status: "queued", CreatedAt: now, UpdatedAt: now, QueuedAt: now, DurationSec: 60, Version: 1, OriginalFilename: &filename}
	log := vod.DeletionLog{JobID: job.ID, FileKind: "work", PathHash: "test-hash", DeletedAt: now, Reason: "test"}
	for startup := 1; startup <= 2; startup++ {
		// Match main's startup budget, including the entire auto dispatch chain.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := Run(ctx, db, u.String(), config.DatabaseMigrationModeAuto)
		cancel()
		if err != nil {
			t.Fatalf("startup %d: %v", startup, err)
		}
		for _, table := range []string{"vod_jobs", "vod_deletion_logs"} {
			if !db.Migrator().HasTable(table) {
				t.Fatalf("startup %d: missing %s", startup, table)
			}
		}
		if !db.Migrator().HasColumn(&vod.Job{}, "original_filename") {
			t.Fatalf("startup %d: missing original_filename", startup)
		}
		if db.Migrator().HasTable("schema_migrations") {
			t.Fatal("auto unexpectedly created versioned history")
		}
		if startup == 1 {
			if err := db.Create(&job).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&log).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	var got vod.Job
	if err := db.First(&got, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.OriginalFilename == nil || *got.OriginalFilename != filename || got.Status != job.Status || got.Version != job.Version {
		t.Fatalf("job changed after repeated startup: %+v", got)
	}
	var gotLog vod.DeletionLog
	if err := db.First(&gotLog, log.ID).Error; err != nil || gotLog.PathHash != log.PathHash {
		t.Fatalf("deletion audit changed: %+v, %v", gotLog, err)
	}
}
