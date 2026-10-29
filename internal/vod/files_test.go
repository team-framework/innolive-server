package vod

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/google/uuid"
)

func TestUploadSharedGroupPermissions(t *testing.T) {
	// umask is process-wide. Isolate it from concurrent tests and goroutines.
	if os.Getenv("VOD_TEST_UMASK_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestUploadSharedGroupPermissions$", "-test.v")
		cmd.Env = append(os.Environ(), "VOD_TEST_UMASK_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("permission subprocess: %v\n%s", err, output)
		}
		if os.Getenv("TEST_DATABASE_URL") == "" {
			t.Skip("TEST_DATABASE_URL required for upload integration")
		}
		return
	}
	for _, mask := range []int{0022, 0077} {
		previous := syscall.Umask(mask)
		for _, missingRoot := range []bool{false, true} {
			s := testService(t)
			root := s.Config.Root
			if missingRoot {
				s.Config.Root = filepath.Join(root, "new", "vod")
			} else if err := os.Chmod(root, os.ModeSetgid|0770); err != nil {
				t.Fatal(err)
			}
			s.Probe = fakeProbe{duration: 60}
			body, contentType := uploadBody(t, 64, 1)
			response := request(t, handler(s), "POST", "/vod/jobs", uuid.New(), body, contentType)
			if response.Code != 201 {
				t.Fatalf("umask %03o: upload status %d: %s", mask, response.Code, response.Body.String())
			}
			jobs, err := s.store.Load().All(context.Background())
			if err != nil || len(jobs) != 1 {
				t.Fatalf("jobs: %v, %v", jobs, err)
			}
			paths := map[string]os.FileMode{
				s.Config.Root: os.ModeSetgid | 0770,
				filepath.Dir(s.Path(jobs[0], "original")): os.ModeSetgid | 0770,
				s.Path(jobs[0], "original"):               0660,
			}
			if missingRoot {
				paths[filepath.Join(root, "new")] = os.ModeSetgid | 0770
			}
			for path, want := range paths {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if got := info.Mode() & (os.ModePerm | os.ModeSetgid); got != want {
					t.Errorf("umask %03o: %s mode %v, want %v", mask, path, got, want)
				}
			}
		}
		syscall.Umask(previous)
	}
}
