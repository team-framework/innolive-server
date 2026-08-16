package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	revokeReasonRotated       = "rotated"
	revokeReasonLogout        = "logout"
	revokeReasonReuseDetected = "reuse_detected"
	revokeReasonUserInactive  = "user_inactive"
	revokeReasonExpired       = "expired"
)

type refreshSessionRow struct {
	ID           uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	UserID       uuid.UUID  `gorm:"column:user_id;type:uuid;not null"`
	FamilyID     uuid.UUID  `gorm:"column:family_id;type:uuid;not null"`
	TokenHash    []byte     `gorm:"column:token_hash;type:bytea;not null"`
	UserAgent    *string    `gorm:"column:user_agent"`
	IPAddress    *string    `gorm:"column:ip_address"`
	ExpiresAt    time.Time  `gorm:"column:expires_at;not null"`
	LastUsedAt   *time.Time `gorm:"column:last_used_at"`
	RevokedAt    *time.Time `gorm:"column:revoked_at"`
	RevokeReason *string    `gorm:"column:revoke_reason"`
	ReplacedByID *uuid.UUID `gorm:"column:replaced_by_id;type:uuid"`
	CreatedAt    time.Time  `gorm:"column:created_at;not null"`
}

func (refreshSessionRow) TableName() string { return "refresh_sessions" }

type gormRefreshStore struct {
	db *gorm.DB
}

func (s *gormRefreshStore) Create(ctx context.Context, record refreshSessionRecord) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureActiveUser(tx, record.UserID); err != nil {
			return err
		}
		return tx.Create(rowFromRecord(record)).Error
	})
}

func (s *gormRefreshStore) Rotate(
	ctx context.Context,
	tokenHash []byte,
	now time.Time,
	next refreshRotation,
) (refreshSessionRecord, error) {
	var nextRecord refreshSessionRecord
	var semanticErr error

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current refreshSessionRow
		result := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("token_hash = ?", tokenHash).
			Take(&current)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			semanticErr = ErrInvalidRefreshToken
			return nil
		}
		if result.Error != nil {
			return result.Error
		}

		switch {
		case current.ReplacedByID != nil:
			if err := touchSession(tx, current.ID, now); err != nil {
				return err
			}
			if err := revokeFamily(tx, current.FamilyID, now, revokeReasonReuseDetected); err != nil {
				return err
			}
			semanticErr = ErrRefreshTokenReused
			return nil
		case current.RevokedAt != nil:
			semanticErr = ErrRefreshTokenRevoked
			return nil
		case !now.Before(current.ExpiresAt):
			if err := revokeSession(tx, current.ID, now, revokeReasonExpired, true); err != nil {
				return err
			}
			semanticErr = ErrRefreshTokenExpired
			return nil
		}

		if err := ensureActiveUser(tx, current.UserID); err != nil {
			if errors.Is(err, ErrUserInactive) {
				if touchErr := touchSession(tx, current.ID, now); touchErr != nil {
					return touchErr
				}
				if revokeErr := revokeFamily(tx, current.FamilyID, now, revokeReasonUserInactive); revokeErr != nil {
					return revokeErr
				}
				semanticErr = ErrUserInactive
				return nil
			}
			return err
		}

		var first refreshSessionRow
		if err := tx.Where("family_id = ?", current.FamilyID).
			Order("created_at ASC").
			Take(&first).Error; err != nil {
			return err
		}

		nextExpiresAt := earlierTime(
			next.IdleExpiresAt,
			first.CreatedAt.Add(next.AbsoluteTTL),
		)
		if !now.Before(nextExpiresAt) {
			if err := revokeSession(tx, current.ID, now, revokeReasonExpired, true); err != nil {
				return err
			}
			semanticErr = ErrRefreshTokenExpired
			return nil
		}

		nextRow := refreshSessionRow{
			ID:        next.ID,
			UserID:    current.UserID,
			FamilyID:  current.FamilyID,
			TokenHash: next.TokenHash,
			UserAgent: tokenOptionalString(next.Client.UserAgent),
			IPAddress: tokenOptionalString(next.Client.IPAddress),
			ExpiresAt: nextExpiresAt,
			CreatedAt: now,
		}
		if err := tx.Create(&nextRow).Error; err != nil {
			return err
		}
		if err := tx.Model(&refreshSessionRow{}).
			Where("id = ? AND revoked_at IS NULL", current.ID).
			Updates(map[string]any{
				"last_used_at":   now,
				"revoked_at":     now,
				"revoke_reason":  revokeReasonRotated,
				"replaced_by_id": next.ID,
			}).Error; err != nil {
			return err
		}
		nextRecord = recordFromRow(nextRow)
		return nil
	})
	if err != nil {
		return refreshSessionRecord{}, err
	}
	if semanticErr != nil {
		return refreshSessionRecord{}, semanticErr
	}
	return nextRecord, nil
}

func (s *gormRefreshStore) RevokeFamilyByHash(ctx context.Context, tokenHash []byte, now time.Time) (uuid.UUID, error) {
	var semanticErr error
	var userID uuid.UUID
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current refreshSessionRow
		result := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("token_hash = ?", tokenHash).
			Take(&current)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			semanticErr = ErrInvalidRefreshToken
			return nil
		}
		if result.Error != nil {
			return result.Error
		}
		// 이미 회전·폐기된 옛 토큰으로는 활성 세션을 종료하면 안 된다. 이
		// 분기는 HTTP 계층이 LogoutUser의 성공 여부로 세션 종료 콜백을
		// 실행하므로, 유효한 현재 토큰으로만 로그아웃 효과를 전파한다.
		if current.RevokedAt != nil {
			semanticErr = ErrRefreshTokenRevoked
			return nil
		}
		if !now.Before(current.ExpiresAt) {
			semanticErr = ErrRefreshTokenExpired
			return nil
		}
		userID = current.UserID
		if err := touchSession(tx, current.ID, now); err != nil {
			return err
		}
		return revokeFamily(tx, current.FamilyID, now, revokeReasonLogout)
	})
	if err != nil {
		return uuid.Nil, err
	}
	if semanticErr != nil {
		return uuid.Nil, semanticErr
	}
	return userID, nil
}

func ensureActiveUser(tx *gorm.DB, userID uuid.UUID) error {
	var row struct {
		Status UserStatus `gorm:"column:status"`
	}
	result := tx.Model(&User{}).
		Select("status").
		Where("id = ?", userID).
		Take(&row)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return ErrUserInactive
	}
	if result.Error != nil {
		return result.Error
	}
	if row.Status != UserStatusActive {
		return ErrUserInactive
	}
	return nil
}

func touchSession(tx *gorm.DB, id uuid.UUID, now time.Time) error {
	return tx.Model(&refreshSessionRow{}).
		Where("id = ?", id).
		Update("last_used_at", now).Error
}

func revokeSession(tx *gorm.DB, id uuid.UUID, now time.Time, reason string, touch bool) error {
	updates := map[string]any{
		"revoked_at":    now,
		"revoke_reason": reason,
	}
	if touch {
		updates["last_used_at"] = now
	}
	return tx.Model(&refreshSessionRow{}).
		Where("id = ? AND revoked_at IS NULL", id).
		Updates(updates).Error
}

func revokeFamily(tx *gorm.DB, familyID uuid.UUID, now time.Time, reason string) error {
	return tx.Model(&refreshSessionRow{}).
		Where("family_id = ? AND revoked_at IS NULL", familyID).
		Updates(map[string]any{
			"revoked_at":    now,
			"revoke_reason": reason,
		}).Error
}

func rowFromRecord(record refreshSessionRecord) *refreshSessionRow {
	return &refreshSessionRow{
		ID:           record.ID,
		UserID:       record.UserID,
		FamilyID:     record.FamilyID,
		TokenHash:    record.TokenHash,
		UserAgent:    record.UserAgent,
		IPAddress:    record.IPAddress,
		ExpiresAt:    record.ExpiresAt,
		LastUsedAt:   record.LastUsedAt,
		RevokedAt:    record.RevokedAt,
		RevokeReason: record.RevokeReason,
		ReplacedByID: record.ReplacedByID,
		CreatedAt:    record.CreatedAt,
	}
}

func recordFromRow(row refreshSessionRow) refreshSessionRecord {
	return refreshSessionRecord{
		ID:           row.ID,
		UserID:       row.UserID,
		FamilyID:     row.FamilyID,
		TokenHash:    row.TokenHash,
		UserAgent:    row.UserAgent,
		IPAddress:    row.IPAddress,
		ExpiresAt:    row.ExpiresAt,
		LastUsedAt:   row.LastUsedAt,
		RevokedAt:    row.RevokedAt,
		RevokeReason: row.RevokeReason,
		ReplacedByID: row.ReplacedByID,
		CreatedAt:    row.CreatedAt,
	}
}
