package vod

import (
	"errors"
	"os"
	"path/filepath"
)

// mkdirShared sets permissions on every directory we create, including a missing
// VOD root. Existing parents retain their operator-managed ownership and modes.
// Go represents setgid with ModeSetgid, not the Unix numeric 02000 bit.
func mkdirShared(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return &os.PathError{Op: "mkdir", Path: path, Err: errors.New("not a directory")}
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := mkdirShared(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0770); err != nil {
		return err
	}
	return os.Chmod(path, os.ModeSetgid|0770)
}
