// Package billing은 결제 채널(앱스토어·원스토어·Paddle)이 공통으로 쓰는 구독
// 원장이다(#422). 채널별 웹훅·영수증 검증은 이 원장에 SubscriptionEvent를 넣는
// 어댑터로 붙고, 사용자 플랜(users.plan)은 여기서만 결제 상태로 다시 계산한다.
package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/plan"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Channel은 결제가 일어난 곳이다.
type Channel string

const (
	ChannelPaddle   Channel = "paddle"
	ChannelAppStore Channel = "app_store"
	ChannelOneStore Channel = "onestore"
)

func (c Channel) Valid() bool {
	switch c {
	case ChannelPaddle, ChannelAppStore, ChannelOneStore:
		return true
	}
	return false
}

// Status는 구독 상태다. canceled는 해지 예약으로, 기간이 끝날 때까지 플랜을 쓴다.
type Status string

const (
	StatusActive   Status = "active"
	StatusCanceled Status = "canceled"
	StatusExpired  Status = "expired"
)

func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusCanceled, StatusExpired:
		return true
	}
	return false
}

// Subscription은 결제 채널의 구독 하나다. 카드·토큰 같은 결제 수단 정보는 두지
// 않는다 — 결제사가 보관하고 여기는 식별자와 상태만 둔다.
type Subscription struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey"`
	UserID     uuid.UUID  `gorm:"type:uuid;not null;index"`
	User       *auth.User `gorm:"foreignKey:UserID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
	Channel    Channel    `gorm:"type:varchar(20);not null;uniqueIndex:uidx_billing_subscriptions_external,priority:1;check:chk_billing_subscriptions_channel,channel IN ('paddle','app_store','onestore')"`
	ExternalID string     `gorm:"type:varchar(255);not null;uniqueIndex:uidx_billing_subscriptions_external,priority:2"`
	Plan       plan.Plan  `gorm:"type:varchar(20);not null;check:chk_billing_subscriptions_plan,plan IN ('glow','beam','plasma')"`
	Status     Status     `gorm:"type:varchar(20);not null;check:chk_billing_subscriptions_status,status IN ('active','canceled','expired')"`
	// CurrentPeriodEnd까지 플랜을 쓴다. 갱신되면 채널 이벤트가 이 값을 민다.
	CurrentPeriodEnd time.Time `gorm:"not null;index"`
	CreatedAt        time.Time `gorm:"not null"`
	UpdatedAt        time.Time `gorm:"not null"`
}

func (Subscription) TableName() string { return "billing_subscriptions" }

// Event는 처리한 채널 이벤트 기록이다. (채널, 이벤트 ID) 고유 키로 같은 웹훅의
// 재전송을 한 번만 반영한다.
type Event struct {
	ID              uuid.UUID `gorm:"type:uuid;primaryKey"`
	Channel         Channel   `gorm:"type:varchar(20);not null;uniqueIndex:uidx_billing_events_external,priority:1"`
	ExternalEventID string    `gorm:"type:varchar(255);not null;uniqueIndex:uidx_billing_events_external,priority:2"`
	// 탈퇴하면 NULL이 된다 — 처리 기록은 남기고 개인과의 연결만 끊는다.
	UserID                 *uuid.UUID `gorm:"type:uuid;index"`
	User                   *auth.User `gorm:"foreignKey:UserID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL"`
	Type                   string     `gorm:"type:varchar(64);not null"`
	SubscriptionExternalID string     `gorm:"type:varchar(255);not null"`
	ReceivedAt             time.Time  `gorm:"not null"`
}

func (Event) TableName() string { return "billing_events" }

// AutoMigrate는 DATABASE_MIGRATION_MODE=auto용이다. users 테이블을 참조하므로
// auth.AutoMigrate 뒤에 부른다.
func AutoMigrate(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return errors.New("GORM database is nil")
	}
	if err := db.WithContext(ctx).AutoMigrate(&Subscription{}, &Event{}); err != nil {
		return fmt.Errorf("auto migrate billing schema: %w", err)
	}
	return nil
}
