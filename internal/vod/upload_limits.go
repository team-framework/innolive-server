package vod

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"syscall"

	"github.com/google/uuid"
)

const mib = 1 << 20

// admitUpload은 업로드를 받기 전에 동시 업로드 수, 사용자의 끝나지 않은 작업 수,
// 디스크 여유를 차례로 본다(V-1). 통과하면 업로드가 끝날 때 부를 release를 돌려준다.
// 제한 값이 0이면 그 제한은 걸지 않는다.
func (s *Service) admitUpload(w http.ResponseWriter, ctx context.Context, db *Store, user uuid.UUID) (func(), bool) {
	release, code := s.acquireUpload(user)
	if code != "" {
		w.Header().Set("Retry-After", "30")
		fail(w, 429, code)
		return nil, false
	}
	if limit := s.Config.MaxActiveJobsPerUser; limit > 0 {
		active, err := db.CountActive(ctx, user)
		if err != nil {
			release()
			fail(w, 503, "service_unavailable")
			return nil, false
		}
		if active >= int64(limit) {
			release()
			w.Header().Set("Retry-After", "60")
			fail(w, 429, "too_many_jobs", map[string]any{"max_active_jobs": limit})
			return nil, false
		}
	}
	free, err := s.FreeBytes(s.Config.Root)
	if err != nil {
		release()
		fail(w, 503, "service_unavailable")
		return nil, false
	}
	// 업로드 상한과 multipart 여유를 받고도 예약분이 남아야 한다.
	need := uint64(s.Config.MaxUploadBytes) + mib + uint64(s.Config.DiskReserveMiB)*mib
	if free < need {
		release()
		fail(w, 507, "insufficient_storage")
		return nil, false
	}
	return release, true
}

// acquireUpload은 동시 업로드 자리를 잡는다. 자리가 없으면 오류 코드를 돌려준다.
func (s *Service) acquireUpload(user uuid.UUID) (func(), string) {
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	if limit := s.Config.MaxUploadsPerUser; limit > 0 && s.uploads[user] >= limit {
		return nil, "upload_in_progress"
	}
	if limit := s.Config.MaxConcurrentUploads; limit > 0 && s.uploading >= limit {
		return nil, "uploads_busy"
	}
	if s.uploads == nil {
		s.uploads = make(map[uuid.UUID]int)
	}
	s.uploads[user]++
	s.uploading++
	released := false
	return func() {
		s.uploadMu.Lock()
		defer s.uploadMu.Unlock()
		if released {
			return
		}
		released = true
		if s.uploads[user]--; s.uploads[user] <= 0 {
			delete(s.uploads, user)
		}
		s.uploading--
	}, ""
}

// diskFreeBytes는 path가 있는 파일 시스템에서 일반 사용자가 쓸 수 있는 바이트다.
// VOD 루트는 첫 업로드 때 만들어지므로, 아직 없으면 가장 가까운 상위 디렉터리를 본다.
func diskFreeBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	for {
		err := syscall.Statfs(path, &stat)
		if err == nil {
			return uint64(stat.Bavail) * uint64(stat.Bsize), nil
		}
		parent := filepath.Dir(path)
		if !errors.Is(err, syscall.ENOENT) || parent == path {
			return 0, err
		}
		path = parent
	}
}

func isNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}
