package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"inno-live-server/internal/plan"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// InnoLive 계정(이메일·비밀번호)을 먼저 만들고 구글·애플은 그 계정에 연결한다(#380).
var (
	// ErrAccountNotLinked는 어느 InnoLive 계정에도 연결되지 않은 구글·애플 신원이다.
	// 로그인은 사용자를 만들지 않고 이 오류로 거절한다.
	ErrAccountNotLinked = errors.New("identity is not linked to an InnoLive account")
	// ErrProviderAlreadyLinked는 사용자에게 다른 애플 신원이 이미 있다. 애플은 계정당 1개다.
	ErrProviderAlreadyLinked = errors.New("provider is already linked to this account")
	// ErrGoogleLinkLimit은 구글 로그인 연결이 상한(maxGoogleLogins)에 닿았다(#392).
	ErrGoogleLinkLimit = errors.New("google login link limit reached")
	// ErrMultipleLinks는 같은 공급자 연결이 여러 개라 해제할 연결 ID가 필요하다.
	ErrMultipleLinks = errors.New("multiple links for provider; link id required")
	// ErrIdentityLinkedElsewhere는 신원이 이메일 계정을 가진 다른 사용자에 붙어 있다.
	ErrIdentityLinkedElsewhere = errors.New("identity is linked to another InnoLive account")
	// ErrIdentityNotLinked는 해제하려는 공급자가 연결돼 있지 않다.
	ErrIdentityNotLinked = errors.New("provider is not linked to this account")
	// ErrLastLoginMethod는 해제하면 로그인할 수단이 남지 않는다.
	ErrLastLoginMethod = errors.New("cannot unlink the last login method")
)

// maxGoogleLogins는 InnoLive 계정 하나에 연결할 수 있는 구글 로그인 수다(#392).
const maxGoogleLogins = 5

// PasswordSetupRequiredError는 연결된 신원이지만 사용자에게 이메일 계정이 없는
// 경우다(#380 이전 OAuth 전용 가입자). 이메일 계정 설정용 짧은 토큰을 싣는다.
type PasswordSetupRequiredError struct {
	SetupToken string
}

func (e *PasswordSetupRequiredError) Error() string { return "password setup required" }

// LinkIdentity는 로그인한 사용자에 붙일 공급자 신원이다.
type LinkIdentity struct {
	Provider        OAuthProvider
	Subject         string
	Email           string
	EmailVerified   bool
	IsPrivateEmail  bool
	RefreshToken    []byte
	RefreshTokenKey *int16
}

// LinkResult는 연결 결과다. MergedUserID는 이메일 계정 없는 다른 사용자를 합쳤을
// 때 사라진 사용자다 — 호출자가 그 사용자의 메모리 세션을 닫는다.
type LinkResult struct {
	Provider     OAuthProvider `json:"provider"`
	MergedUserID *uuid.UUID    `json:"-"`
}

// LoginMethods는 사용자가 로그인에 쓸 수 있는 수단이다.
type LoginMethods struct {
	Email     *string        `json:"email"`
	Providers []LinkedMethod `json:"providers"`
}

type LinkedMethod struct {
	ID       uuid.UUID     `json:"id"`
	Provider OAuthProvider `json:"provider"`
	Email    *string       `json:"email"`
	LinkedAt time.Time     `json:"linked_at"`
}

type AccountLinkStore struct {
	db  *gorm.DB
	now func() time.Time
}

func NewAccountLinkStore(db *gorm.DB) *AccountLinkStore {
	return &AccountLinkStore{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// Link는 신원을 userID에 붙인다. 신원이 이메일 계정 없는 다른 사용자에 붙어 있으면
// 그 사용자를 userID로 합친다(#380 결정 a). 한 트랜잭션이라 중간 실패는 남지 않는다.
func (s *AccountLinkStore) Link(ctx context.Context, userID uuid.UUID, identity LinkIdentity) (LinkResult, error) {
	if s == nil || s.db == nil {
		return LinkResult{}, errors.New("account link database is nil")
	}
	result := LinkResult{Provider: identity.Provider}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockActiveUser(tx, userID); err != nil {
			return err
		}
		now := s.now()
		var existing OAuthAccount
		query := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("provider = ? AND provider_subject = ?", identity.Provider, identity.Subject).
			Take(&existing)
		switch {
		case errors.Is(query.Error, gorm.ErrRecordNotFound):
			if err := ensureProviderFree(tx, userID, identity.Provider, 1); err != nil {
				return err
			}
			account := newLinkedOAuthAccount(userID, identity, now)
			return tx.Create(&account).Error
		case query.Error != nil:
			return query.Error
		case existing.UserID == userID:
			return tx.Model(&OAuthAccount{}).Where("id = ?", existing.ID).Updates(linkedAccountUpdates(identity, now)).Error
		}
		hasEmail, err := userHasEmailAccount(tx, existing.UserID)
		if err != nil {
			return err
		}
		if hasEmail {
			return ErrIdentityLinkedElsewhere
		}
		if err := mergeUser(tx, existing.UserID, userID, now); err != nil {
			return err
		}
		if err := tx.Model(&OAuthAccount{}).Where("id = ?", existing.ID).Updates(linkedAccountUpdates(identity, now)).Error; err != nil {
			return err
		}
		merged := existing.UserID
		result.MergedUserID = &merged
		return nil
	})
	if err != nil {
		return LinkResult{}, err
	}
	return result, nil
}

// Unlink는 공급자 연결을 끊는다. 이메일 계정이 없으면 로그인 수단이 사라지므로 거절한다.
// linkID가 nil이면 그 공급자 연결이 하나일 때만 끊는다 — 여러 개면 ErrMultipleLinks.
func (s *AccountLinkStore) Unlink(ctx context.Context, userID uuid.UUID, provider OAuthProvider, linkID *uuid.UUID) error {
	if s == nil || s.db == nil {
		return errors.New("account link database is nil")
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockActiveUser(tx, userID); err != nil {
			return err
		}
		hasEmail, err := userHasEmailAccount(tx, userID)
		if err != nil {
			return err
		}
		if !hasEmail {
			return ErrLastLoginMethod
		}
		scope := tx.Where("user_id = ? AND provider = ?", userID, provider)
		if linkID != nil {
			scope = scope.Where("id = ?", *linkID)
		} else {
			var count int64
			if err := tx.Model(&OAuthAccount{}).Where("user_id = ? AND provider = ?", userID, provider).Count(&count).Error; err != nil {
				return err
			}
			if count > 1 {
				return ErrMultipleLinks
			}
		}
		deleted := scope.Delete(&OAuthAccount{})
		if deleted.Error != nil {
			return deleted.Error
		}
		if deleted.RowsAffected == 0 {
			return ErrIdentityNotLinked
		}
		return nil
	})
}

// Methods는 사용자의 이메일 계정과 연결된 공급자를 돌려준다.
func (s *AccountLinkStore) Methods(ctx context.Context, userID uuid.UUID) (LoginMethods, error) {
	if s == nil || s.db == nil {
		return LoginMethods{}, errors.New("account link database is nil")
	}
	db := s.db.WithContext(ctx)
	methods := LoginMethods{Providers: []LinkedMethod{}}
	var email EmailAccount
	switch err := db.Where("user_id = ?", userID).Take(&email).Error; {
	case err == nil:
		methods.Email = &email.Email
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return LoginMethods{}, err
	}
	var accounts []OAuthAccount
	if err := db.Where("user_id = ?", userID).Order("provider, created_at").Find(&accounts).Error; err != nil {
		return LoginMethods{}, err
	}
	for _, account := range accounts {
		methods.Providers = append(methods.Providers, LinkedMethod{ID: account.ID, Provider: account.Provider, Email: account.ProviderEmail, LinkedAt: account.CreatedAt})
	}
	return methods, nil
}

func lockActiveUser(tx *gorm.DB, userID uuid.UUID) error {
	var user User
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "status").Where("id = ?", userID).Take(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrUserInactive
	}
	if err != nil {
		return err
	}
	if user.Status != UserStatusActive {
		return ErrUserInactive
	}
	return nil
}

// ensureProviderFree는 userID에 provider 신원 adding개를 더 붙일 수 있는지 본다.
// 애플은 계정당 1개, 구글은 maxGoogleLogins개까지다(#392). 호출자가 사용자 행을
// 잠근 트랜잭션 안에서 부르므로 세기와 추가 사이에 다른 연결이 끼어들지 않는다.
func ensureProviderFree(tx *gorm.DB, userID uuid.UUID, provider OAuthProvider, adding int) error {
	var count int64
	if err := tx.Model(&OAuthAccount{}).Where("user_id = ? AND provider = ?", userID, provider).Count(&count).Error; err != nil {
		return err
	}
	if provider == OAuthProviderGoogle {
		if int(count)+adding > maxGoogleLogins {
			return ErrGoogleLinkLimit
		}
		return nil
	}
	if count != 0 {
		return ErrProviderAlreadyLinked
	}
	return nil
}

func userHasEmailAccount(tx *gorm.DB, userID uuid.UUID) (bool, error) {
	var count int64
	if err := tx.Model(&EmailAccount{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		return false, err
	}
	return count != 0, nil
}

// mergeUser는 이메일 계정이 없는 from 사용자를 into로 합치고 from을 지운다(#380 결정 a).
//   - OAuth 신원: 모두 into로 옮긴다. 애플이 양쪽에 있거나 구글이 상한을 넘으면 병합 전체를 거절한다
//   - 송출 계정: into에 없는 플랫폼만 옮기고, 겹치면 into 것을 남긴다
//   - 사용 기록(usage_sessions): into로 옮긴다
//   - 플랜: 둘 중 높은 것
//
// from의 refresh 세션은 사용자 행 삭제와 함께 cascade로 지워진다.
func mergeUser(tx *gorm.DB, from, into uuid.UUID, now time.Time) error {
	var fromUser, intoUser User
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", from).Take(&fromUser).Error; err != nil {
		return fmt.Errorf("lock merged user: %w", err)
	}
	if err := tx.Where("id = ?", into).Take(&intoUser).Error; err != nil {
		return fmt.Errorf("read merge target: %w", err)
	}

	var fromAccounts []OAuthAccount
	if err := tx.Where("user_id = ?", from).Find(&fromAccounts).Error; err != nil {
		return err
	}
	adding := map[OAuthProvider]int{}
	for _, account := range fromAccounts {
		adding[account.Provider]++
	}
	for provider, count := range adding {
		if err := ensureProviderFree(tx, into, provider, count); err != nil {
			return err
		}
	}
	if err := tx.Model(&OAuthAccount{}).Where("user_id = ?", from).Updates(map[string]any{"user_id": into, "updated_at": now}).Error; err != nil {
		return fmt.Errorf("move oauth accounts: %w", err)
	}

	if err := tx.Model(&StreamingAccount{}).
		Where("user_id = ? AND provider NOT IN (?)", from, tx.Model(&StreamingAccount{}).Select("provider").Where("user_id = ?", into)).
		Update("user_id", into).Error; err != nil {
		return fmt.Errorf("move streaming accounts: %w", err)
	}

	// usage 패키지가 auth에 의존하므로 모델 대신 테이블 이름으로 옮긴다. 사용 기록을
	// 쓰지 않는 배포(테이블 없음)는 건너뛴다.
	if tx.Migrator().HasTable("usage_sessions") {
		if err := tx.Exec("UPDATE usage_sessions SET user_id = ? WHERE user_id = ?", into, from).Error; err != nil {
			return fmt.Errorf("move usage sessions: %w", err)
		}
	}

	if higherPlan(fromUser.Plan, intoUser.Plan) {
		if err := tx.Model(&User{}).Where("id = ?", into).Updates(map[string]any{"plan": fromUser.Plan, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("carry merged plan: %w", err)
		}
	}
	if err := tx.Where("id = ?", from).Delete(&User{}).Error; err != nil {
		return fmt.Errorf("delete merged user: %w", err)
	}
	return nil
}

var planRank = map[plan.Plan]int{plan.Spark: 0, plan.Glow: 1, plan.Beam: 2, plan.Plasma: 3}

func higherPlan(candidate, current plan.Plan) bool {
	return planRank[candidate] > planRank[current]
}

func newLinkedOAuthAccount(userID uuid.UUID, identity LinkIdentity, now time.Time) OAuthAccount {
	return OAuthAccount{
		ID:                             uuid.New(),
		UserID:                         userID,
		Provider:                       identity.Provider,
		ProviderSubject:                identity.Subject,
		ProviderEmail:                  googleOptionalString(identity.Email, 320),
		EmailVerified:                  identity.EmailVerified,
		IsPrivateEmail:                 identity.IsPrivateEmail,
		ProviderRefreshTokenCiphertext: identity.RefreshToken,
		ProviderTokenKeyVersion:        identity.RefreshTokenKey,
		LastLoginAt:                    now,
		CreatedAt:                      now,
		UpdatedAt:                      now,
	}
}

func linkedAccountUpdates(identity LinkIdentity, now time.Time) map[string]any {
	updates := map[string]any{
		"provider_email":   googleOptionalString(identity.Email, 320),
		"email_verified":   identity.EmailVerified,
		"is_private_email": identity.IsPrivateEmail,
		"updated_at":       now,
	}
	if len(identity.RefreshToken) > 0 {
		updates["provider_refresh_token_ciphertext"] = identity.RefreshToken
		updates["provider_token_key_version"] = identity.RefreshTokenKey
	}
	return updates
}
