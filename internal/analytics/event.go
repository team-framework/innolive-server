package analytics

import (
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"io"
	"math"
	"regexp"
)

// The trusted gateway owns event-specific privacy and semantics. This collector
// enforces only a bounded common envelope; new names need no Go DTO or migration.
type Event struct {
	Version    int            `json:"version"`
	EventID    string         `json:"eventId"`
	VisitID    string         `json:"visitId"`
	Sequence   int64          `json:"sequence"`
	Event      string         `json:"event"`
	Properties map[string]any `json:"properties"`
	Release    string         `json:"release"`
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var controlPattern = regexp.MustCompile(`[\x00-\x1f]`)
var releasePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

func validID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.Version() == 4 && id.Variant() == uuid.RFC4122 && id.String() == s
}
func Decode(r io.Reader) (Event, error) {
	var e Event
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(&e); err != nil {
		return e, errors.New("invalid event")
	}
	var extra any
	if d.Decode(&extra) != io.EOF || e.Version != 1 || !validID(e.EventID) || !validID(e.VisitID) || e.Sequence < 1 || e.Sequence > 1000000 || !namePattern.MatchString(e.Event) || e.Properties == nil || len(e.Properties) > 32 {
		return e, errors.New("invalid event")
	}
	for key, value := range e.Properties {
		if !namePattern.MatchString(key) {
			return e, errors.New("invalid properties")
		}
		switch v := value.(type) {
		case bool:
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 9007199254740991 {
				return e, errors.New("invalid number")
			}
		case string:
			if len(v) > 128 || controlPattern.MatchString(v) {
				return e, errors.New("invalid string")
			}
		default:
			return e, errors.New("invalid property type")
		}
	}
	if e.Release == "" {
		e.Release = "unknown"
	}
	if !releasePattern.MatchString(e.Release) {
		return e, errors.New("invalid release")
	}
	return e, nil
}
