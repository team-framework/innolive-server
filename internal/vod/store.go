package vod

import (
	"context"
	_ "embed"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

//go:embed schema.sql
var schemaSQL string

//go:embed filename.sql
var filenameSQL string

func AutoMigrate(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(schemaSQL).Error; err != nil {
			return err
		}
		return tx.Exec(filenameSQL).Error
	})
}

type Job struct {
	ID               uuid.UUID `gorm:"type:uuid;primaryKey"`
	UserID           uuid.UUID `gorm:"type:uuid"`
	Status           string
	Progress         int
	CreatedAt        time.Time
	UpdatedAt        time.Time
	QueuedAt         time.Time
	CompletedAt      *time.Time
	OriginalDeleteAt *time.Time
	ResultExpiresAt  *time.Time
	OriginalFilename *string
	DeletedAt        *time.Time
	DurationSec      float64
	FramesDone       uint64
	FramesTotal      uint64
	SegmentsDone     uint32
	ProcessingFPS    float64 `gorm:"column:processing_fps"`
	CheckpointToken  string
	WorkerTarget     string
	FailureCode      string
	Version          int64
}

func (Job) TableName() string { return "vod_jobs" }

var terminalStatuses = []string{"done", "failed", "cancelled", "expired"}

func (j Job) Terminal() bool {
	return slices.Contains(terminalStatuses, j.Status)
}

type DeletionLog struct {
	ID        int64
	JobID     uuid.UUID `gorm:"type:uuid"`
	FileKind  string
	PathHash  string
	DeletedAt time.Time
	Reason    string
}

func (DeletionLog) TableName() string { return "vod_deletion_logs" }

var ErrConflict = errors.New("VOD job changed concurrently")

type Store struct{ DB *gorm.DB }

func NewStore(db *gorm.DB) *Store {
	return &Store{DB: db.Session(&gorm.Session{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})}
}
func (s *Store) Create(ctx context.Context, j *Job) error {
	return s.DB.WithContext(ctx).Create(j).Error
}
func (s *Store) Get(ctx context.Context, id, user uuid.UUID) (Job, error) {
	var j Job
	err := s.DB.WithContext(ctx).Where("id = ? AND user_id = ? AND deleted_at IS NULL", id, user).Take(&j).Error
	return j, err
}

// CountActive는 사용자의 끝나지 않은 작업 수다.
func (s *Store) CountActive(ctx context.Context, user uuid.UUID) (int64, error) {
	var n int64
	err := s.DB.WithContext(ctx).Model(&Job{}).
		Where("user_id = ? AND deleted_at IS NULL AND status NOT IN ?", user, terminalStatuses).Count(&n).Error
	return n, err
}
func (s *Store) All(ctx context.Context) ([]Job, error) {
	var jobs []Job
	err := s.DB.WithContext(ctx).Order("queued_at, id").Find(&jobs).Error
	return jobs, err
}

// Version compare-and-swap keeps late worker messages from resurrecting deleted jobs.
func (s *Store) Save(ctx context.Context, j *Job) error {
	old := j.Version
	j.Version++
	r := s.DB.WithContext(ctx).Model(&Job{}).Where("id = ? AND version = ?", j.ID, old).Select("*").Updates(j)
	if r.Error != nil {
		j.Version = old
		return r.Error
	}
	if r.RowsAffected != 1 {
		j.Version = old
		return ErrConflict
	}
	return nil
}
func (s *Store) RecordDeletion(ctx context.Context, l *DeletionLog) error {
	return s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(l).Error
}
