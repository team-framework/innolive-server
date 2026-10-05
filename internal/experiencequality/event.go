package experiencequality

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Event contains only bounded measurement values, never user or media identifiers.
type Event struct {
	Version   int     `json:"version"`
	AttemptID string  `json:"attemptId"`
	Event     string  `json:"event"`
	Role      string  `json:"role"`
	Locale    string  `json:"locale"`
	Retry     *bool   `json:"retry"`
	Stage     string  `json:"stage"`
	ElapsedMS *int64  `json:"elapsedMs"`
	Code      *string `json:"code,omitempty"`
	Browser   string  `json:"browser,omitempty"`
	Release   string  `json:"release,omitempty"`
}

var releasePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

func Decode(reader io.Reader) (Event, error) {
	var e Event
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&e); err != nil {
		return e, errors.New("invalid event")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return e, errors.New("invalid event")
	}
	id, err := uuid.Parse(e.AttemptID)
	if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || strings.ToLower(e.AttemptID) != id.String() ||
		e.Version != 1 || e.Retry == nil || e.ElapsedMS == nil || *e.ElapsedMS < 0 || *e.ElapsedMS > 86400000 ||
		!oneOf(e.Event, "started", "transport_connected", "first_frame", "failed", "ended", "cancelled") ||
		!oneOf(e.Stage, "session", "queue", "camera", "signaling", "transport", "first_frame", "streaming") ||
		!oneOf(e.Role, "member", "guest") || !oneOf(e.Locale, "ko", "en", "ja") {
		return e, errors.New("invalid event")
	}
	if e.Event == "failed" {
		if e.Code == nil || !oneOf(*e.Code, "permission_denied", "camera_missing", "timeout", "request_failed", "connection_failed") {
			return e, errors.New("invalid failure code")
		}
	} else if e.Code != nil {
		return e, errors.New("unexpected failure code")
	}
	if e.Browser == "" {
		e.Browser = "unknown"
	}
	if !oneOf(e.Browser, "safari", "chrome", "firefox", "edge", "other", "unknown") {
		return e, errors.New("invalid browser")
	}
	if e.Release == "" {
		e.Release = "unknown"
	}
	if !releasePattern.MatchString(e.Release) {
		return e, errors.New("invalid release")
	}
	return e, nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
