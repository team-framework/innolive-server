package experiencequality

import (
	"context"
	_ "embed"
	"log/slog"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

const Retention = 30 * 24 * time.Hour

//go:embed schema.sql
var schemaSQL string

func AutoMigrate(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Exec(schemaSQL).Error
}

type Row struct {
	AttemptID  string `gorm:"type:uuid;primaryKey"`
	Event      string `gorm:"primaryKey"`
	Version    int
	Role       string
	Locale     string
	Retry      bool
	Stage      string
	ElapsedMS  int64
	Code       *string
	Browser    string
	Release    string
	ReceivedAt time.Time
}

func (Row) TableName() string { return "experience_quality_events" }

type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store {
	// Database errors must not copy an event's values into application SQL logs.
	return &Store{db: db.Session(&gorm.Session{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})}
}

func (s *Store) Save(ctx context.Context, e Event) error {
	row := Row{AttemptID: e.AttemptID, Event: e.Event, Version: e.Version, Role: e.Role, Locale: e.Locale, Retry: *e.Retry,
		Stage: e.Stage, ElapsedMS: *e.ElapsedMS, Code: e.Code, Browser: e.Browser, Release: e.Release, ReceivedAt: time.Now().UTC()}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "attempt_id"}, {Name: "event"}}, DoNothing: true}).Create(&row).Error
}

// Purge removes a bounded batch. Duplicates never extend the original retention time.
func (s *Store) Purge(ctx context.Context, now time.Time) (int64, error) {
	result := s.db.WithContext(ctx).Exec(`DELETE FROM experience_quality_events WHERE (attempt_id, event) IN
 (SELECT attempt_id, event FROM experience_quality_events WHERE received_at < ? ORDER BY received_at LIMIT 1000)`, now.Add(-Retention))
	return result.RowsAffected, result.Error
}

func (s *Store) RunRetention(ctx context.Context, logger *slog.Logger) {
	sweep := func() {
		sweepCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for batch := 0; batch < 100; batch++ {
			count, err := s.Purge(sweepCtx, time.Now().UTC())
			if err != nil {
				if ctx.Err() == nil {
					logger.Warn("experience quality retention failed")
				}
				return
			}
			if count < 1000 {
				return
			}
		}
	}
	sweep()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
