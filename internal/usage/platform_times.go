package usage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// PlatformTimesStore는 송출 세대에 플랫폼의 실제 방송 시각을 남긴다(#437).
type PlatformTimesStore struct {
	db *gorm.DB
}

func NewPlatformTimesStore(db *gorm.DB) *PlatformTimesStore {
	return &PlatformTimesStore{db: db}
}

// errBroadcastNotRecorded는 송출 세대 행이 아직 없다는 뜻이다. 실사용 기록은
// 비동기라 늦을 수 있다.
var errBroadcastNotRecorded = errors.New("usage broadcast not recorded")

// RecordPlatformTimes는 usageID 송출 세대에 플랫폼 방송 ID와 실제 시각을 쓴다.
func (s *PlatformTimesStore) RecordPlatformTimes(ctx context.Context, usageID, platformBroadcastID string, started, ended *time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("platform times store is nil")
	}
	result := s.db.WithContext(ctx).Model(&Broadcast{}).Where("id = ?", usageID).Updates(map[string]any{
		"platform_broadcast_id": platformBroadcastID,
		"platform_started_at":   started,
		"platform_ended_at":     ended,
	})
	if result.Error != nil {
		return fmt.Errorf("record platform times: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return errBroadcastNotRecorded
	}
	return nil
}
