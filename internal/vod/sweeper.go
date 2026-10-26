package vod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
		if !j.Terminal() || j.WorkerTarget != "" {
			continue
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
	return errors.Join(errs...)
}
func (s *Service) purge(ctx context.Context, j Job, kind, reason string) error {
	path := s.Path(j, kind)
	// Only generated UUID paths are used. RemoveAll never follows symlinks.
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	relative, err := filepath.Rel(s.Config.Root, path)
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
					if j.Status == "done" && j.ResultExpiresAt != nil {
						remaining := j.ResultExpiresAt.Sub(s.Now())
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
