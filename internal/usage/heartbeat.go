package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// HeartbeatRetention은 하트비트를 모두 남기는 기간이다(P1-33). 지난 행은 세션마다
// 가장 늦은 한 건만 남긴다 — 원장은 비정상 종료 송출의 끝을 세션별 마지막 하트비트로
// 정하므로(lastHeartbeats) 압축해도 차감이 바뀌지 않는다.
const HeartbeatRetention = 30 * 24 * time.Hour

const heartbeatCompactBatch = 1000

// Compact는 보존 기간이 지난 하트비트 중 같은 세션에 더 늦은 행이 있는 것을 최대
// heartbeatCompactBatch건 지우고 지운 수를 돌려준다.
func (s *HeartbeatStore) Compact(ctx context.Context, now time.Time) (int64, error) {
	result := s.db.WithContext(ctx).Exec(`DELETE FROM usage_heartbeats WHERE id IN (
 SELECT old.id FROM usage_heartbeats old WHERE old.received_at < ? AND EXISTS (
  SELECT 1 FROM usage_heartbeats later WHERE later.session_id = old.session_id AND later.received_at > old.received_at)
 LIMIT ?)`, now.Add(-HeartbeatRetention), heartbeatCompactBatch)
	if result.Error != nil {
		return 0, fmt.Errorf("compact heartbeats: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// RunRetention은 기동 직후와 이후 한 시간마다 Compact를 돌린다. ctx가 끝나면 멈춘다.
// 두 슬롯이 함께 돌려도 같은 행을 지울 뿐이다.
func (s *HeartbeatStore) RunRetention(ctx context.Context, logger *slog.Logger) {
	sweep := func() {
		sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		for batch := 0; batch < 100; batch++ {
			count, err := s.Compact(sweepCtx, time.Now().UTC())
			if err != nil {
				if ctx.Err() == nil {
					logger.Warn("usage heartbeat retention failed", "error", err)
				}
				return
			}
			if count < heartbeatCompactBatch {
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
