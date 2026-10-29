package vod

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Original filenames are display metadata only, never filesystem paths or logs.
func sanitizeOriginalFilename(name string) *string {
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	name = strings.ToValidUTF8(name, "\uFFFD")
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if len(name) > 255 {
		end := 255
		for !utf8.RuneStart(name[end]) {
			end--
		}
		name = strings.TrimSpace(name[:end])
	}
	if name == "" || name == "." || name == ".." {
		return nil
	}
	return &name
}

func (j Job) filenameExpired(now time.Time) bool {
	return j.DeletedAt != nil || j.Status == "failed" || j.Status == "cancelled" || j.Status == "expired" ||
		(j.ResultExpiresAt != nil && !now.Before(*j.ResultExpiresAt))
}
