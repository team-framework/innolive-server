// Package usage는 스트리머의 실사용(세션·송출) 기록을 PostgreSQL에 남긴다(#266).
// 파생 지표(직전 방송 이후 간격, 재방문율 등)는 저장하지 않고 조회 시점에
// 계산한다 — scripts/usage/report.sql 참고.
package usage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"inno-live-server/internal/auth"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	SourceLive     = "live"
	SourceBackfill = "backfill"

	// ReasonUncleanShutdown은 프로세스가 종료를 기록하지 못하고 죽은 행에 기동 시
	// 붙이는 사유다. 이 행의 ended_at은 NULL로 남아 시간 통계에서 빠진다.
	ReasonUncleanShutdown = "unclean_shutdown"
)

// Session은 WebRTC 세션 하나(앱을 켠 구간)다.
type Session struct {
	ID uuid.UUID `gorm:"column:session_id;type:uuid;primaryKey"`
	// 탈퇴하면 NULL이 된다 — 집계는 남기고 개인과의 연결만 끊는다.
	UserID       *uuid.UUID `gorm:"type:uuid;index:idx_usage_sessions_user_started,priority:1"`
	User         *auth.User `gorm:"foreignKey:UserID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL"`
	IsGuest      bool       `gorm:"not null"`
	AIProcessing *string    `gorm:"column:ai_processing;type:varchar(20)"`
	StartedAt    time.Time  `gorm:"not null;index:idx_usage_sessions_user_started,priority:2"`
	EndedAt      *time.Time
	EndReason    *string `gorm:"type:varchar(64)"`
	Source       string  `gorm:"type:varchar(10);not null;default:live;check:chk_usage_sessions_source,source IN ('live','backfill')"`
	// 관계는 이쪽(has-many)에 둔다. Broadcast 쪽에 belongs-to로 두면 양쪽 모두
	// session_id 컬럼을 가져 GORM이 방향을 거꾸로 추정한다.
	Broadcasts []Broadcast `gorm:"foreignKey:SessionID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (Session) TableName() string { return "usage_sessions" }

// Broadcast는 플랫폼 송출(egress) 한 세대다. 동시 송출이면 한 세션에 여럿이다.
type Broadcast struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	SessionID uuid.UUID `gorm:"type:uuid;not null;index"`
	Provider  string    `gorm:"type:varchar(20);not null"`
	StartedAt time.Time `gorm:"not null"`
	// LiveAt은 플랫폼이 라이브로 전환된 시각이다. 유튜브는 준비 단계에서 송출이
	// 먼저 붙으므로 실제 방송 시간은 이 시각부터 센다.
	LiveAt        *time.Time
	EndedAt       *time.Time
	EndReason     *string `gorm:"type:varchar(64)"`
	PausedSeconds float64 `gorm:"type:double precision;not null;default:0"`
	Source        string  `gorm:"type:varchar(10);not null;default:live;check:chk_usage_broadcasts_source,source IN ('live','backfill')"`
	// 백필에서 종료 로그가 없어 세션 종료 시각으로 채운 행이다.
	EndedAtEstimated bool `gorm:"not null;default:false"`
}

func (Broadcast) TableName() string { return "usage_broadcasts" }

// AutoMigrate는 DATABASE_MIGRATION_MODE=auto용이다. users 테이블을 참조하므로
// auth.AutoMigrate 뒤에 부른다.
func AutoMigrate(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return errors.New("GORM database is nil")
	}
	if err := db.WithContext(ctx).AutoMigrate(&Session{}, &Broadcast{}); err != nil {
		return fmt.Errorf("auto migrate usage schema: %w", err)
	}
	return nil
}

// CloseOrphans는 직전 프로세스가 종료를 기록하지 못한 행을 마감한다. 새 세션이
// 생기기 전, 기동 시 한 번 부른다.
func CloseOrphans(ctx context.Context, db *gorm.DB) error {
	for _, model := range []any{&Broadcast{}, &Session{}} {
		if err := db.WithContext(ctx).Model(model).
			Where("ended_at IS NULL AND end_reason IS NULL AND source = ?", SourceLive).
			Update("end_reason", ReasonUncleanShutdown).Error; err != nil {
			return fmt.Errorf("close orphaned usage rows: %w", err)
		}
	}
	return nil
}
