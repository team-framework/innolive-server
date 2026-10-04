package analytics

import (
	"context"
	_ "embed"
	"encoding/json"
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
	EventID    string `gorm:"type:uuid;primaryKey"`
	VisitID    string `gorm:"type:uuid"`
	Sequence   int64
	Version    int
	Event      string
	Properties string `gorm:"type:jsonb"`
	Release    string
	ReceivedAt time.Time
}

func (Row) TableName() string { return "analytics_events" }

type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store {
	// Database errors must not copy an event's values into application SQL logs.
	return &Store{db: db.Session(&gorm.Session{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})}
}

func (s *Store) Save(ctx context.Context, e Event) error {
	properties, err := json.Marshal(e.Properties)
	if err != nil {
		return err
	}
	row := Row{EventID: e.EventID, VisitID: e.VisitID, Sequence: e.Sequence, Version: e.Version, Event: e.Event, Properties: string(properties), Release: e.Release, ReceivedAt: time.Now().UTC()}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "event_id"}}, DoNothing: true}).Create(&row).Error
}

// Purge removes a bounded batch. Duplicates never extend the original retention time.
func (s *Store) Purge(ctx context.Context, now time.Time) (int64, error) {
	result := s.db.WithContext(ctx).Exec(`DELETE FROM analytics_events WHERE event_id IN
 (SELECT event_id FROM analytics_events WHERE received_at < ? ORDER BY received_at LIMIT 1000)`, now.Add(-Retention))
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
					logger.Warn("analytics retention failed")
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
