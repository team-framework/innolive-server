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
	MarkUserDeleted(context.Context, uuid.UUID, time.Time) error
}

// WithdrawalCleanup contains the external and in-memory cleanup stages that
// must succeed before the account rows are deleted. Each callback must
// leave its database rows intact until all external work for that stage has
// completed so a later request can retry it.
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

		// Remove every account-owned row in one transaction. Authentication rejects
		// old access tokens because the user lookup below will no longer find a
		// row, while provider subjects and email addresses become available for a
		// future account.
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

// MarkDeleted closes the in-process operation gate after the database delete
// commits. It prevents a request that passed active-user authentication just
// before withdrawal from writing a new session or reference face afterwards.
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

// SetCleanup wires the server-owned stages after all platform services and the
// HTTP server have been assembled. It must be called before serving requests.
func (s *AccountWithdrawalService) SetCleanup(cleanup WithdrawalCleanup) {
	if s == nil {
		return
	}
	s.cleanup = cleanup
}

// BeginOperation admits a user-scoped operation. Server and auth services use
// this method to close the race between withdrawal cleanup and new data writes.
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
