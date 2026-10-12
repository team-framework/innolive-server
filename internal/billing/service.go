package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"inno-live-server/internal/plan"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	// ErrInvalidEvent는 채널 어댑터가 넘긴 이벤트가 필수 값을 빠뜨렸거나 모르는
	// 값을 담은 경우다.
	ErrInvalidEvent = errors.New("invalid billing event")
	// ErrSubscriptionOwnerMismatch는 이미 다른 사용자에게 붙은 구독 ID로 이벤트가
	// 온 경우다. 구독을 다른 계정으로 옮기지 않고 거절한다.
	ErrSubscriptionOwnerMismatch = errors.New("subscription belongs to another user")
	// ErrUserNotFound는 구독을 붙일 사용자가 없는 경우다.
	ErrUserNotFound = errors.New("billing user not found")
)

// SubscriptionEvent는 채널 어댑터가 결제사 알림을 해석해 넘기는 구독의 최신
// 상태다. 결제사가 이벤트를 순서 없이 보낼 수 있으므로 어댑터는 알림 본문이
// 아니라 결제사에서 다시 읽은 현재 상태를 담는다.
type SubscriptionEvent struct {
	Channel          Channel
	EventID          string
	Type             string
	UserID           uuid.UUID
	ExternalID       string
	Plan             plan.Plan
	Status           Status
	CurrentPeriodEnd time.Time
}

// Result는 이벤트 적용 결과다.
type Result struct {
	// Duplicate면 이미 처리한 이벤트라 아무것도 바꾸지 않았다.
	Duplicate bool
	// Plan은 적용 뒤 사용자의 플랜이다. Duplicate면 비어 있다.
	Plan plan.Plan
}

// Service는 구독 원장에 이벤트를 적용하고 사용자 플랜을 다시 계산한다.
type Service struct {
	db  *gorm.DB
	now func() time.Time
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db, now: time.Now}
}

// Apply는 이벤트 하나를 한 트랜잭션으로 적용한다: 중복 검사, 구독 갱신, 플랜
// 재계산. 같은 (채널, 이벤트 ID)는 두 번째부터 Duplicate로 끝난다.
func (s *Service) Apply(ctx context.Context, event SubscriptionEvent) (Result, error) {
	if err := validate(event); err != nil {
		return Result{}, err
	}
	now := s.now().UTC()
	var result Result
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockUser(tx, event.UserID); err != nil {
			return err
		}
		userID := event.UserID
		inserted := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&Event{
			ID:                     uuid.New(),
			Channel:                event.Channel,
			ExternalEventID:        event.EventID,
			UserID:                 &userID,
			Type:                   event.Type,
			SubscriptionExternalID: event.ExternalID,
			ReceivedAt:             now,
		})
		if inserted.Error != nil {
			return fmt.Errorf("record billing event: %w", inserted.Error)
		}
		if inserted.RowsAffected == 0 {
			result.Duplicate = true
			return nil
		}
		if err := upsertSubscription(tx, event, now); err != nil {
			return err
		}
		value, err := recomputePlan(tx, event.UserID, now)
		if err != nil {
			return err
		}
		result.Plan = value
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

// ExpireDue는 기간이 끝난 구독을 만료로 바꾸고 해당 사용자들의 플랜을 다시
// 계산한다. 바꾼 구독 수를 돌려준다.
func (s *Service) ExpireDue(ctx context.Context) (int, error) {
	now := s.now().UTC()
	var userIDs []uuid.UUID
	if err := s.db.WithContext(ctx).Model(&Subscription{}).
		Where("status IN ? AND current_period_end <= ?", []Status{StatusActive, StatusCanceled}, now).
		Distinct().Pluck("user_id", &userIDs).Error; err != nil {
		return 0, fmt.Errorf("find due subscriptions: %w", err)
	}
	expired := 0
	for _, userID := range userIDs {
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := lockUser(tx, userID); err != nil {
				return err
			}
			updated := tx.Model(&Subscription{}).
				Where("user_id = ? AND status IN ? AND current_period_end <= ?", userID, []Status{StatusActive, StatusCanceled}, now).
				Updates(map[string]any{"status": StatusExpired, "updated_at": now})
			if updated.Error != nil {
				return fmt.Errorf("expire subscriptions: %w", updated.Error)
			}
			expired += int(updated.RowsAffected)
			_, err := recomputePlan(tx, userID, now)
			return err
		})
		// 그사이 탈퇴한 사용자는 구독도 함께 지워졌으므로 건너뛴다.
		if err != nil && !errors.Is(err, ErrUserNotFound) {
			return expired, err
		}
	}
	return expired, nil
}

// RunExpiry는 ctx가 끝날 때까지 주기적으로 ExpireDue를 돈다.
func (s *Service) RunExpiry(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	sweep := func() {
		sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		count, err := s.ExpireDue(sweepCtx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("billing expiry failed", "error", err)
			}
			return
		}
		if count > 0 {
			logger.Info("billing subscriptions expired", "count", count)
		}
	}
	sweep()
	ticker := time.NewTicker(interval)
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

func validate(event SubscriptionEvent) error {
	switch {
	case !event.Channel.Valid():
		return fmt.Errorf("%w: unknown channel %q", ErrInvalidEvent, event.Channel)
	case strings.TrimSpace(event.EventID) == "" || len(event.EventID) > 255:
		return fmt.Errorf("%w: event id", ErrInvalidEvent)
	case strings.TrimSpace(event.Type) == "" || len(event.Type) > 64:
		return fmt.Errorf("%w: event type", ErrInvalidEvent)
	case event.UserID == uuid.Nil:
		return fmt.Errorf("%w: user id", ErrInvalidEvent)
	case strings.TrimSpace(event.ExternalID) == "" || len(event.ExternalID) > 255:
		return fmt.Errorf("%w: subscription id", ErrInvalidEvent)
	case !event.Plan.Valid() || event.Plan == plan.Spark:
		return fmt.Errorf("%w: plan %q", ErrInvalidEvent, event.Plan)
	case !event.Status.Valid():
		return fmt.Errorf("%w: status %q", ErrInvalidEvent, event.Status)
	case event.CurrentPeriodEnd.IsZero():
		return fmt.Errorf("%w: period end", ErrInvalidEvent)
	}
	return nil
}

// lockUser는 사용자 행을 잠근다. 같은 사용자의 이벤트·만료 처리가 동시에 돌아도
// 플랜 재계산이 차례로 일어난다.
func lockUser(tx *gorm.DB, userID uuid.UUID) error {
	var found []uuid.UUID
	if err := tx.Table("users").Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", userID).Pluck("id", &found).Error; err != nil {
		return fmt.Errorf("lock billing user: %w", err)
	}
	if len(found) == 0 {
		return ErrUserNotFound
	}
	return nil
}

func upsertSubscription(tx *gorm.DB, event SubscriptionEvent, now time.Time) error {
	var existing Subscription
	err := tx.Where("channel = ? AND external_id = ?", event.Channel, event.ExternalID).Take(&existing).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		if err := tx.Create(&Subscription{
			ID:               uuid.New(),
			UserID:           event.UserID,
			Channel:          event.Channel,
			ExternalID:       event.ExternalID,
			Plan:             event.Plan,
			Status:           event.Status,
			CurrentPeriodEnd: event.CurrentPeriodEnd.UTC(),
			CreatedAt:        now,
			UpdatedAt:        now,
		}).Error; err != nil {
			return fmt.Errorf("create subscription: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("read subscription: %w", err)
	case existing.UserID != event.UserID:
		return ErrSubscriptionOwnerMismatch
	}
	if err := tx.Model(&Subscription{}).Where("id = ?", existing.ID).Updates(map[string]any{
		"plan":               event.Plan,
		"status":             event.Status,
		"current_period_end": event.CurrentPeriodEnd.UTC(),
		"updated_at":         now,
	}).Error; err != nil {
		return fmt.Errorf("update subscription: %w", err)
	}
	return nil
}

var planRank = map[plan.Plan]int{plan.Spark: 0, plan.Glow: 1, plan.Beam: 2, plan.Plasma: 3}

// recomputePlan은 기간이 남은 활성·해지 예약 구독 중 가장 높은 플랜을 사용자
// 플랜으로 쓴다. 남은 구독이 없으면 Spark다. 구독 기록이 아예 없는 사용자는
// 여기까지 오지 않으므로 관리자가 지정한 플랜이 유지된다.
func recomputePlan(tx *gorm.DB, userID uuid.UUID, now time.Time) (plan.Plan, error) {
	var plans []plan.Plan
	if err := tx.Model(&Subscription{}).
		Where("user_id = ? AND status IN ? AND current_period_end > ?", userID, []Status{StatusActive, StatusCanceled}, now).
		Pluck("plan", &plans).Error; err != nil {
		return "", fmt.Errorf("read entitled subscriptions: %w", err)
	}
	value := plan.Spark
	for _, candidate := range plans {
		if planRank[candidate] > planRank[value] {
			value = candidate
		}
	}
	if err := tx.Table("users").Where("id = ?", userID).
		Updates(map[string]any{"plan": value, "updated_at": now}).Error; err != nil {
		return "", fmt.Errorf("apply billing plan: %w", err)
	}
	return value, nil
}
