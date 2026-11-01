package usage

import (
	"context"
	"fmt"
	"time"

	"inno-live-server/internal/auth"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"
)

// LocalTake는 서버 없이 iOS 로컬 모드로 기기에 저장한 촬영 한 건이다. 앱이 재연결
// 때 올리고, 같은 take_id는 다시 올려도 한 번만 남는다. 송출 대상이 없으므로
// 온디바이스 시간에서 1배로 차감한다.
type LocalTake struct {
	TakeID          uuid.UUID  `gorm:"column:take_id;type:uuid;primaryKey"`
	UserID          *uuid.UUID `gorm:"type:uuid;index:idx_usage_local_takes_user_started,priority:1"`
	User            *auth.User `gorm:"foreignKey:UserID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL"`
	DeviceID        string     `gorm:"type:varchar(64);not null"`
	StartedAt       time.Time  `gorm:"not null;index:idx_usage_local_takes_user_started,priority:2"`
	EndedAt         time.Time  `gorm:"not null"`
	DurationSeconds int64      `gorm:"not null"`
	ReceivedAt      time.Time  `gorm:"not null"`
}

func (LocalTake) TableName() string { return "usage_local_takes" }

// RecordLocalTakes는 이미 있는 take_id를 건너뛴다. 다른 사용자의 take_id와
// 겹쳐도 기존 행을 바꾸지 않는다.
func (l *Ledger) RecordLocalTakes(ctx context.Context, takes []LocalTake) error {
	if len(takes) == 0 {
		return nil
	}
	if err := l.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "take_id"}}, DoNothing: true}).
		Create(&takes).Error; err != nil {
		return fmt.Errorf("record local takes: %w", err)
	}
	return nil
}

// localTakeCharges는 시작 시각이 [from, to) 안인 로컬 촬영이다. 달을 걸친 촬영은
// 시작한 달에 모두 센다.
func (l *Ledger) localTakeCharges(ctx context.Context, userID uuid.UUID, from, to time.Time) ([]SessionCharge, error) {
	var takes []LocalTake
	if err := l.db.WithContext(ctx).Where("user_id = ? AND started_at >= ? AND started_at < ?", userID, from, to).
		Order("started_at").Find(&takes).Error; err != nil {
		return nil, fmt.Errorf("read local takes: %w", err)
	}
	charges := make([]SessionCharge, 0, len(takes))
	for _, take := range takes {
		duration := time.Duration(take.DurationSeconds) * time.Second
		charges = append(charges, SessionCharge{SessionID: take.TakeID, StartedAt: take.StartedAt, Providers: []string{}, OnDevice: true, Local: true, OnAir: duration, Charged: duration})
	}
	return charges, nil
}
