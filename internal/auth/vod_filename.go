package auth

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// VOD rows outlive accounts. Erase display metadata and mark every job deleted in
// the withdrawal transaction even when VOD is disabled, without depending on the
// vod package. The deletion mirrors DELETE /vod/jobs/{id}: the scheduler cancels
// worker runs and the sweeper destroys originals and results. The purge deadline
// is the withdrawal time, so the sweeper does not wait for a worker ack (V-2).
func deleteVODJobs(tx *gorm.DB, userID uuid.UUID, now time.Time) error {
	var columns struct {
		Table    bool
		Filename bool
	}
	if err := tx.Raw(`SELECT to_regclass('vod_jobs') IS NOT NULL AS table, EXISTS (
		SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('vod_jobs')
		AND attname = 'original_filename' AND NOT attisdropped
	) AS filename`).Scan(&columns).Error; err != nil {
		return err
	}
	// Support databases predating the optional VOD migrations.
	if !columns.Table {
		return nil
	}
	if columns.Filename {
		if err := tx.Exec(`UPDATE vod_jobs SET original_filename = NULL, version = version + 1
			WHERE user_id = ? AND original_filename IS NOT NULL`, userID).Error; err != nil {
			return err
		}
	}
	return tx.Exec(`UPDATE vod_jobs SET
		deleted_at = COALESCE(deleted_at, @now),
		status = CASE WHEN status IN ('done','failed','cancelled','expired') THEN status ELSE 'cancelled' END,
		completed_at = COALESCE(completed_at, @now),
		original_delete_at = LEAST(COALESCE(original_delete_at, @now), @now),
		updated_at = @now,
		version = version + 1
		WHERE user_id = @user AND (deleted_at IS NULL OR original_delete_at IS NULL OR original_delete_at > @now)`,
		map[string]any{"now": now, "user": userID}).Error
}
