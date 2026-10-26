package vod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/google/uuid"
	"os"
	"path/filepath"
	"time"
)

// Sweep removes originals and work immediately after acknowledged termination,
// leaving the configured deadline as an upper bound. Failed deletes retry on
// every sweep. Results remain until expiry; HTTP independently enforces expiry.
func (s *Service) Sweep(ctx context.Context) error {
	if !s.Config.Enabled {
		return nil
	}
	db := s.store.Load()
	if db == nil {
		return nil
	}
	jobs, err := db.All(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, j := range jobs {
		if !j.Terminal() {
			continue
		}
		if j.WorkerTarget != "" {
			if j.OriginalDeleteAt == nil || s.Now().Before(*j.OriginalDeleteAt) {
				continue
			}
			// A failed worker acknowledgement cannot extend the promised
			// filesystem retention deadline. Keep retrying cancellation, but
			// remove its server-owned paths now and report the missing ack.
			s.Logger.Error("vod_retention_worker_unconfirmed", "job_id", j.ID, "deadline", *j.OriginalDeleteAt)
		}
		reason := "processing_completed"
		if j.DeletedAt != nil {
			reason = "user_deleted"
		} else if j.Status == "failed" {
			reason = "processing_failed"
		} else if j.Status == "cancelled" {
			reason = "cancelled"
		}
		clean := true
		for _, kind := range []string{"original", "work", "partial"} {
			if err := s.purge(ctx, j, kind, reason); err != nil {
				errs = append(errs, err)
				clean = false
			}
		}
		expired := j.ResultExpiresAt != nil && !s.Now().Before(*j.ResultExpiresAt)
		if expired || j.DeletedAt != nil || j.Status == "failed" || j.Status == "cancelled" {
			resultReason := reason
			if expired {
				resultReason = "result_expired"
			}
			if err := s.purge(ctx, j, "result", resultReason); err != nil {
				errs = append(errs, err)
				clean = false
			}
			if clean && expired && j.DeletedAt == nil && j.Status == "done" {
				j.Status = "expired"
				j.UpdatedAt = s.Now()
				if err := db.Save(ctx, &j); err != nil && !errors.Is(err, ErrConflict) {
					errs = append(errs, err)
				}
			}
		}
	}
	if err := s.sweepOrphans(ctx, jobs); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
func (s *Service) purge(ctx context.Context, j Job, kind, reason string) error {
	path := s.Path(j, kind)
	relative, err := filepath.Rel(s.Config.Root, path)
	if err != nil {
		return err
	}
	// OpenRoot also rejects a job directory symlink escaping the shared root.
	root, err := os.OpenRoot(s.Config.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.RemoveAll(relative); err != nil {
		return err
	}
	relative, err = filepath.Rel(s.Config.Root, path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(relative))
	entry := DeletionLog{JobID: j.ID, FileKind: kind, PathHash: hex.EncodeToString(sum[:]), DeletedAt: s.Now(), Reason: reason}
	if err := s.store.Load().RecordDeletion(ctx, &entry); err != nil {
		return err
	}
	if entry.ID != 0 {
		s.Logger.Info("vod_file_destroyed", "job_id", j.ID, "file_kind", kind, "path_hash", entry.PathHash, "deleted_at", entry.DeletedAt, "reason", reason)
	}
	return nil
}

func (s *Service) RunSweeper(ctx context.Context) {
	if !s.Config.Enabled {
		return
	}
	for ctx.Err() == nil {
		if err := s.Sweep(ctx); err != nil {
			s.Logger.Error("vod retention retry required", "error", err)
		}
		wait := s.Config.SweepInterval
		if db := s.store.Load(); db != nil {
			jobs, err := db.All(ctx)
			if err == nil {
				for _, j := range jobs {
					for _, deadline := range []*time.Time{j.OriginalDeleteAt, j.ResultExpiresAt} {
						if deadline == nil {
							continue
						}
						remaining := deadline.Sub(s.Now())
						if remaining > 0 && remaining < wait {
							wait = remaining
						}
					}
				}
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.sweepWake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// A process can die after writing input but before accepting a DB row. Only UUID
// directories older than the entire upload deadline plus one sweep are eligible.
func (s *Service) sweepOrphans(ctx context.Context, jobs []Job) error {
	entries, err := os.ReadDir(s.Config.Root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	known := map[uuid.UUID]bool{}
	for _, j := range jobs {
		known[j.ID] = true
	}
	for _, entry := range entries {
		id, err := uuid.Parse(entry.Name())
		if err != nil || known[id] || !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if s.Now().Sub(info.ModTime()) <= s.Config.UploadTimeout+s.Config.SweepInterval {
			continue
		}
		// Re-read after the directory scan so completed uploads do not become orphans
		// merely because they committed after the initial job snapshot.
		var count int64
		if err := s.store.Load().DB.WithContext(ctx).Model(&Job{}).Where("id = ?", id).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			continue
		}
		j := Job{ID: id}
		for _, kind := range []string{"original", "work", "partial", "result"} {
			if err := s.purge(ctx, j, kind, "abandoned_upload"); err != nil {
				return err
			}
		}
		root, err := os.OpenRoot(s.Config.Root)
		if err != nil {
			return err
		}
		err = root.Remove(entry.Name())
		_ = root.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
