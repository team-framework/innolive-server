package vod

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"mime/multipart"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"syscall"
	"testing"

	"inno-live-server/internal/config"

	"github.com/google/uuid"
)

func limitUpload(t *testing.T, s *Service, user uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	b := new(bytes.Buffer)
	m := multipart.NewWriter(b)
	headers := textproto.MIMEHeader{}
	headers.Set("Content-Disposition", `form-data; name="file"; filename="clip.mp4"`)
	headers.Set("Content-Type", "video/mp4")
	p, e := m.CreatePart(headers)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = p.Write([]byte("video"))
	_ = m.Close()
	return request(t, handler(s), "POST", "/vod/jobs", user, b, m.FormDataContentType())
}

func TestUploadRejectsUserWithTooManyActiveJobs(t *testing.T) {
	s := testService(t)
	s.Probe = fakeProbe{60, nil}
	s.FreeBytes = func(string) (uint64, error) { return 1 << 50, nil }
	busy, finished := uuid.New(), uuid.New()
	for i, state := range []string{"uploaded", "queued", "processing"} {
		j := createJob(t, s, state)
		j.UserID = busy
		saveJob(t, s, &j)
		done := createJob(t, s, []string{"done", "failed", "cancelled"}[i])
		done.UserID = finished
		saveJob(t, s, &done)
	}
	w := limitUpload(t, s, busy)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), `"code":"too_many_jobs"`) {
		t.Fatalf("busy user = %d %q: %s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	// 끝난 작업은 세지 않는다.
	if w := limitUpload(t, s, finished); w.Code != 201 {
		t.Fatalf("user with only finished jobs = %d: %s", w.Code, w.Body.String())
	}
}

func TestUploadRejectsWhenDiskIsShort(t *testing.T) {
	s := testService(t)
	s.Probe = fakeProbe{60, nil}
	s.Config.MaxUploadBytes = 32
	need := uint64(32) + mib + uint64(s.Config.DiskReserveMiB)*mib
	s.FreeBytes = func(string) (uint64, error) { return need - 1, nil }
	if w := limitUpload(t, s, uuid.New()); w.Code != 507 || !strings.Contains(w.Body.String(), `"code":"insufficient_storage"`) {
		t.Fatalf("short disk = %d: %s", w.Code, w.Body.String())
	}
	s.FreeBytes = func(string) (uint64, error) { return 0, errors.New("statfs failed") }
	if w := limitUpload(t, s, uuid.New()); w.Code != 503 {
		t.Fatalf("statfs failure = %d: %s", w.Code, w.Body.String())
	}
	if entries, _ := os.ReadDir(s.Config.Root); len(entries) != 0 {
		t.Fatal("rejected upload left files")
	}
	s.FreeBytes = func(string) (uint64, error) { return need, nil }
	if w := limitUpload(t, s, uuid.New()); w.Code != 201 {
		t.Fatalf("enough disk = %d: %s", w.Code, w.Body.String())
	}
}

func TestAcquireUploadLimitsPerUserAndOverall(t *testing.T) {
	cfg := config.DefaultVOD()
	cfg.MaxUploadsPerUser, cfg.MaxConcurrentUploads = 1, 2
	s := &Service{Config: cfg}
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	releaseA, code := s.acquireUpload(a)
	if code != "" {
		t.Fatal(code)
	}
	if _, code := s.acquireUpload(a); code != "upload_in_progress" {
		t.Fatalf("second upload by same user = %q", code)
	}
	if _, code := s.acquireUpload(b); code != "" {
		t.Fatal(code)
	}
	if _, code := s.acquireUpload(c); code != "uploads_busy" {
		t.Fatalf("third concurrent upload = %q", code)
	}
	releaseA()
	releaseA()
	if _, code := s.acquireUpload(c); code != "" {
		t.Fatalf("after release = %q", code)
	}
	if s.uploading != 2 {
		t.Fatalf("uploading = %d after double release, want 2", s.uploading)
	}
}

func TestIsNoSpaceSeesWrappedENOSPC(t *testing.T) {
	if !isNoSpace(&fs.PathError{Op: "write", Path: "input.mp4", Err: syscall.ENOSPC}) || isNoSpace(context.Canceled) || isNoSpace(nil) {
		t.Fatal("ENOSPC detection")
	}
}

func TestDiskFreeBytesUsesNearestExistingParent(t *testing.T) {
	root := t.TempDir()
	existing, err := diskFreeBytes(root)
	if err != nil || existing == 0 {
		t.Fatalf("existing root: %d %v", existing, err)
	}
	if missing, err := diskFreeBytes(root + "/not/yet/created"); err != nil || missing == 0 {
		t.Fatalf("missing root: %d %v", missing, err)
	}
}
