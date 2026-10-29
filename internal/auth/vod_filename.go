package auth

import (
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// VOD rows outlive accounts. Erase display metadata in the withdrawal
// transaction even when VOD is disabled, without depending on the vod package.
func clearVODOriginalFilenames(tx *gorm.DB, userID uuid.UUID) error {
	var exists bool
	if err := tx.Raw(`SELECT EXISTS (
		SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('vod_jobs')
		AND attname = 'original_filename' AND NOT attisdropped
	)`).Scan(&exists).Error; err != nil {
		return err
	}
	// Support databases predating the optional VOD migrations.
	if !exists {
		return nil
	}
	return tx.Exec(`UPDATE vod_jobs SET original_filename = NULL, version = version + 1
		WHERE user_id = ? AND original_filename IS NOT NULL`, userID).Error
}
