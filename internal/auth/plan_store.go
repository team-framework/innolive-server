package auth

import (
	"context"
	"errors"
	"fmt"

	"inno-live-server/internal/plan"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ErrUserNotFound는 플랜을 조회·변경할 사용자가 없는 경우다.
var ErrUserNotFound = errors.New("user not found")

// PlanStore는 사용자의 요금제를 읽고 쓴다(#270). 결제 연동 전에는 관리자
// 지정만이 쓰기 경로다.
type PlanStore struct {
	db *gorm.DB
}

func NewPlanStore(db *gorm.DB) *PlanStore {
	return &PlanStore{db: db}
}

// UserPlan은 사용자의 플랜을 돌려준다. 탈퇴 등으로 비활성인 사용자도 행이
// 있으면 그대로 돌려준다 — 활성 여부는 인증 미들웨어가 이미 본다.
func (s *PlanStore) UserPlan(ctx context.Context, userID uuid.UUID) (plan.Plan, error) {
	var row struct {
		Plan plan.Plan `gorm:"column:plan"`
	}
	result := s.db.WithContext(ctx).Model(&User{}).Select("plan").Where("id = ?", userID).Take(&row)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return "", ErrUserNotFound
	}
	if result.Error != nil {
		return "", fmt.Errorf("read user plan: %w", result.Error)
	}
	return row.Plan, nil
}

// SetUserPlan은 사용자의 플랜을 바꾼다. 진행 중인 세션에는 반영되지 않는다 —
// 세션은 생성 시점의 플랜을 쥔다.
func (s *PlanStore) SetUserPlan(ctx context.Context, userID uuid.UUID, value plan.Plan) error {
	if !value.Valid() {
		return fmt.Errorf("set user plan: unknown plan %q", value)
	}
	result := s.db.WithContext(ctx).Model(&User{}).Where("id = ?", userID).Update("plan", value)
	if result.Error != nil {
		return fmt.Errorf("set user plan: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrUserNotFound
	}
	return nil
}
