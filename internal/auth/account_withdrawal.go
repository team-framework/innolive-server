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

func (s *gormWithdrawalAccountStore) MarkUserDeleted(ctx context.Context, userID uuid.UUID, now time.Time) error {
	if s == nil || s.db == nil {
		return ErrWithdrawalUnavailable
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user User
		result := tx.Where("id = ?", userID).Take(&user)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) || user.Status != UserStatusActive {
			return ErrUserInactive
		}
		if result.Error != nil {
			return result.Error
		}
		if err := tx.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{
			"status": UserStatusDeleted, "email": nil, "display_name": nil,
			"profile_image_url": nil, "updated_at": now,
		}).Error; err != nil {
			return err
		}
		if err := tx.Model(&OAuthAccount{}).Where("user_id = ?", userID).Updates(map[string]any{
			"provider_email": nil, "provider_refresh_token_ciphertext": nil,
			"provider_token_key_version": nil, "updated_at": now,
		}).Error; err != nil {
			return err
		}
		return tx.Model(&RefreshSession{}).Where("user_id = ? AND revoked_at IS NULL", userID).Updates(map[string]any{
			"revoked_at": now, "revoke_reason": "user_withdrawal",
		}).Error
	})
}

type AccountWithdrawalService struct {
	store        WithdrawalAccountStore
	cipher       *ProviderTokenCipher
	apple        AppleTokenRevoker
	now          func() time.Time
	afterDeleted func(uuid.UUID)
}

func NewAccountWithdrawalService(store WithdrawalAccountStore, cipher *ProviderTokenCipher, apple AppleTokenRevoker, afterDeleted func(uuid.UUID)) (*AccountWithdrawalService, error) {
	if store == nil {
		return nil, errors.New("withdrawal account store must not be nil")
	}
	return &AccountWithdrawalService{store: store, cipher: cipher, apple: apple, now: func() time.Time { return time.Now().UTC() }, afterDeleted: afterDeleted}, nil
}

func (s *AccountWithdrawalService) Withdraw(ctx context.Context, userID uuid.UUID) error {
	credential, err := s.store.AppleRevocationCredential(ctx, userID)
	if err != nil {
		return err
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
	if err := s.store.MarkUserDeleted(ctx, userID, s.now().UTC()); err != nil {
		return err
	}
	if s.afterDeleted != nil {
		s.afterDeleted(userID)
	}
	return nil
}
