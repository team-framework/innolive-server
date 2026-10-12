package usage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Heartbeat는 온디바이스 직접 송출 중 앱이 보낸 상태 보고 한 건이다(#434).
// 서버가 영상을 받지 않으므로 직접 송출의 성능·상태는 이 보고가 유일한 출처다.
// 판정에 쓰는 값만 컬럼으로 두고 나머지는 원문(Payload)에 남긴다 — 보고 항목은
// 클라이언트 실측에 따라 바뀔 수 있어 컬럼으로 고정하지 않는다.
type Heartbeat struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`
	// 실사용 기록과 같은 세션 ID다. 실사용 기록은 비동기라 행이 늦거나 빠질 수
	// 있으므로 외래 키를 걸지 않는다 — 보고가 그 때문에 거절되면 안 된다.
	SessionID        uuid.UUID `gorm:"type:uuid;not null;index:idx_usage_heartbeats_session_received,priority:1"`
	ReceivedAt       time.Time `gorm:"not null;index:idx_usage_heartbeats_session_received,priority:2"`
	SentAt           time.Time `gorm:"not null"`
	ConnectedTargets int       `gorm:"not null"`
	VideoFPS         float64   `gorm:"column:video_fps;type:double precision;not null"`
	DroppedFrames    int64     `gorm:"not null"`
	FailedFrames     int64     `gorm:"not null"`
	Platform         string    `gorm:"type:varchar(20);not null"`
	Payload          string    `gorm:"type:jsonb;not null"`
}

func (Heartbeat) TableName() string { return "usage_heartbeats" }

// HeartbeatStore는 하트비트 저장소다.
type HeartbeatStore struct {
	db *gorm.DB
}

func NewHeartbeatStore(db *gorm.DB) *HeartbeatStore {
	return &HeartbeatStore{db: db}
}

func (s *HeartbeatStore) SaveHeartbeat(ctx context.Context, heartbeat Heartbeat) error {
	if s == nil || s.db == nil {
		return errors.New("heartbeat store is nil")
	}
	if heartbeat.ID == uuid.Nil {
		heartbeat.ID = uuid.New()
	}
	if err := s.db.WithContext(ctx).Create(&heartbeat).Error; err != nil {
		return fmt.Errorf("save heartbeat: %w", err)
	}
	return nil
}
