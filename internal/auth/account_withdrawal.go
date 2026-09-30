package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var ErrWithdrawalUnavailable = errors.New("account withdrawal is unavailable")

type appleRevocationCredential struct {
	Ciphertext []byte
	Version    *int16
}

type WithdrawalAccountStore interface {
	AppleRevocationCredential(context.Context, uuid.UUID) (*appleRevocationCredential, error)
	UserState(context.Context, uuid.UUID) (WithdrawalUserState, error)
	MarkUserDeleted(context.Context, uuid.UUID, time.Time) error
}

type WithdrawalUserState int

const (
	WithdrawalUserUnknown WithdrawalUserState = iota
	WithdrawalUserMissing
	WithdrawalUserActive
	WithdrawalUserInactive
)

// WithdrawalCleanup은 계정 행을 지우기 전에 성공해야 하는 외부·메모리 정리 단계를
// 담는다. 각 콜백은 그 단계의 외부 작업이 다 끝날 때까지 DB 행을 남겨, 이후 요청이
// 다시 시도할 수 있게 해야 한다.
type WithdrawalCleanup struct {
	CloseUserSessions           func(context.Context, uuid.UUID) error
	DisconnectStreamingAccounts func(context.Context, uuid.UUID) error
	ClearReferenceData          func(context.Context, uuid.UUID) error
	ClearStreamingTokenCache    func(uuid.UUID)
}

type gormWithdrawalAccountStore struct{ db *gorm.DB }

func NewGormWithdrawalAccountStore(db *gorm.DB) WithdrawalAccountStore {
	return &gormWithdrawalAccountStore{db: db}
}

func (s *gormWithdrawalAccountStore) AppleRevocationCredential(ctx context.Context, userID uuid.UUID) (*appleRevocationCredential, error) {
	if s == nil || s.db == nil {
		return nil, ErrWithdrawalUnavailable
	}
	var account OAuthAccount
	result := s.db.WithContext(ctx).Where("user_id = ? AND provider = ?", userID, OAuthProviderApple).Take(&account)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}
	if len(account.ProviderRefreshTokenCiphertext) == 0 {
		return nil, nil
	}
	return &appleRevocationCredential{Ciphertext: account.ProviderRefreshTokenCiphertext, Version: account.ProviderTokenKeyVersion}, nil
}

func (s *gormWithdrawalAccountStore) UserState(ctx context.Context, userID uuid.UUID) (WithdrawalUserState, error) {
	if s == nil || s.db == nil {
		return WithdrawalUserUnknown, ErrWithdrawalUnavailable
	}
	var user User
	result := s.db.WithContext(ctx).Select("status").Where("id = ?", userID).Take(&user)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return WithdrawalUserMissing, nil
	}
	if result.Error != nil {
		return WithdrawalUserUnknown, result.Error
	}
	if user.Status != UserStatusActive {
		return WithdrawalUserInactive, nil
	}
	return WithdrawalUserActive, nil
}

func (s *gormWithdrawalAccountStore) MarkUserDeleted(ctx context.Context, userID uuid.UUID, _ time.Time) error {
	if s == nil || s.db == nil {
		return ErrWithdrawalUnavailable
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user User
		result := tx.Where("id = ?", userID).Take(&user)
		if result.Error != nil {
			if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				return ErrUserInactive
			}
			return result.Error
		}
		if user.Status != UserStatusActive {
			return ErrUserInactive
		}

		// 계정이 가진 행을 한 트랜잭션에서 모두 지운다. 아래 사용자 조회가 더는 행을
		// 찾지 못하므로 인증은 이전 access token을 거절하고, 플랫폼 subject와 이메일
		// 주소는 이후 새 계정이 쓸 수 있게 된다.
		for _, model := range []any{&EmailAccount{}, &OAuthAccount{}, &StreamingAccount{}, &RefreshSession{}} {
			if err := tx.Where("user_id = ?", userID).Delete(model).Error; err != nil {
				return err
			}
		}
		if result := tx.Where("id = ?", userID).Delete(&User{}); result.Error != nil {
			return result.Error
		} else if result.RowsAffected != 1 {
			return ErrUserInactive
		}
		return nil
	})
}

// MarkDeleted는 DB 삭제가 커밋된 뒤 프로세스 안 작업 게이트를 닫는다. 탈퇴 직전에
// 활성 사용자 인증을 통과한 요청이 그 뒤에 새 세션이나 기준 얼굴을 쓰지 못하게
// 한다.
func (s *AccountWithdrawalService) MarkDeleted(userID uuid.UUID) {
	if s == nil || s.gate == nil {
		return
	}
	s.gate.MarkDeleted(userID)
}

type AccountWithdrawalService struct {
	store        WithdrawalAccountStore
	cipher       *ProviderTokenCipher
	apple        AppleTokenRevoker
	now          func() time.Time
	afterDeleted func(uuid.UUID)
	cleanup      WithdrawalCleanup
	gate         *UserOperationGate
}

func NewAccountWithdrawalService(store WithdrawalAccountStore, cipher *ProviderTokenCipher, apple AppleTokenRevoker, afterDeleted func(uuid.UUID)) (*AccountWithdrawalService, error) {
	if store == nil {
		return nil, errors.New("withdrawal account store must not be nil")
	}
	return &AccountWithdrawalService{
		store:        store,
		cipher:       cipher,
		apple:        apple,
		now:          func() time.Time { return time.Now().UTC() },
		afterDeleted: afterDeleted,
		gate:         NewUserOperationGate(),
	}, nil
}

// SetCleanup은 모든 플랫폼 서비스와 HTTP 서버를 조립한 뒤 서버가 가진 정리 단계를
// 연결한다. 요청을 받기 전에 불러야 한다.
func (s *AccountWithdrawalService) SetCleanup(cleanup WithdrawalCleanup) {
	if s == nil {
		return
	}
	s.cleanup = cleanup
}

// BeginOperation은 사용자 범위 작업을 들인다. 서버와 인증 서비스가 탈퇴 정리와 새
// 데이터 쓰기 사이의 경쟁을 막는 데 쓴다.
func (s *AccountWithdrawalService) BeginOperation(userID uuid.UUID) (func(), bool) {
	if s == nil || s.gate == nil {
		return func() {}, true
	}
	return s.gate.BeginOperation(userID)
}

func (s *AccountWithdrawalService) beginWithdrawal(ctx context.Context, userID uuid.UUID) (func(), error) {
	if s == nil || s.gate == nil {
		return nil, ErrWithdrawalUnavailable
	}
	return s.gate.BeginWithdrawal(ctx, userID)
}

func (s *AccountWithdrawalService) Withdraw(ctx context.Context, userID uuid.UUID) error {
	release, err := s.beginWithdrawal(ctx, userID)
	if err != nil {
		return err
	}
	defer release()

	credential, err := s.store.AppleRevocationCredential(ctx, userID)
	if err != nil {
		return err
	}
	if s.cleanup.CloseUserSessions != nil {
		if err := s.cleanup.CloseUserSessions(ctx, userID); err != nil {
			return fmt.Errorf("close user sessions: %w", err)
		}
	}
	if s.cleanup.DisconnectStreamingAccounts != nil {
		if err := s.cleanup.DisconnectStreamingAccounts(ctx, userID); err != nil {
			return fmt.Errorf("cleanup streaming accounts: %w", err)
		}
	}
	if credential != nil {
		if s.cipher == nil || s.apple == nil {
			return ErrWithdrawalUnavailable
		}
		refreshToken, err := s.cipher.Decrypt(credential.Ciphertext, credential.Version)
		if err != nil {
			return fmt.Errorf("decrypt Apple refresh token: %w", err)
		}
		if err := s.apple.Revoke(ctx, refreshToken); err != nil {
			return fmt.Errorf("revoke Apple refresh token: %w", err)
		}
	}
	if s.cleanup.ClearReferenceData != nil {
		if err := s.cleanup.ClearReferenceData(ctx, userID); err != nil {
			return fmt.Errorf("clear reference data: %w", err)
		}
	}
	if err := s.store.MarkUserDeleted(ctx, userID, s.now().UTC()); err != nil {
		return err
	}
	s.MarkDeleted(userID)
	if s.cleanup.ClearStreamingTokenCache != nil {
		s.cleanup.ClearStreamingTokenCache(userID)
	}
	if s.afterDeleted != nil {
		s.afterDeleted(userID)
	}
	return nil
}

func (s *AccountWithdrawalService) UserState(ctx context.Context, userID uuid.UUID) (WithdrawalUserState, error) {
	if s == nil || s.store == nil {
		return WithdrawalUserUnknown, ErrWithdrawalUnavailable
	}
	return s.store.UserState(ctx, userID)
}
