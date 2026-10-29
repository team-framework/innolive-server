package vod

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"inno-live-server/internal/config"
)

var ErrUnsupported = errors.New("unsupported media")
var ErrInvalidMedia = errors.New("invalid media")

// acceptedProbeContainer allows MP4 and QuickTime/MOV ffprobe format_name values
// (e.g. "mov,mp4,m4a,3gp,3g2,mj2").
func acceptedProbeContainer(formatName string) bool {
	for _, name := range strings.Split(formatName, ",") {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "mp4", "mov", "isom", "iso2", "mp42", "mp41":
			return true
		}
	}
	return false
}

type Media struct {
	Duration float64
	Frames   uint64
}
type Prober interface {
	Probe(context.Context, string) (Media, error)
}
type FFProbe struct{}

func (FFProbe) Probe(ctx context.Context, path string) (Media, error) {
	b, e := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=format_name,duration:stream=codec_type,codec_name,nb_frames,width,height", "-of", "json", path).Output()
	if e != nil {
		return Media{}, ErrInvalidMedia
	}
	var data struct {
		Format struct {
			FormatName string `json:"format_name"`
			Duration   string
		}
		Streams []struct {
			CodecType     string `json:"codec_type"`
			CodecName     string `json:"codec_name"`
			NBFrames      string `json:"nb_frames"`
			Width, Height int
		}
	}
	if json.Unmarshal(b, &data) != nil {
		return Media{}, ErrInvalidMedia
	}
	duration, e := strconv.ParseFloat(data.Format.Duration, 64)
	if e != nil || duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return Media{}, ErrInvalidMedia
	}
	if !acceptedProbeContainer(data.Format.FormatName) {
		return Media{}, ErrUnsupported
	}
	videos := 0
	var frames uint64
	for _, s := range data.Streams {
		switch s.CodecType {
		case "video":
			videos++
			if s.CodecName != "h264" || s.Width > 1920 || s.Height > 1920 {
				return Media{}, ErrUnsupported
			}
			frames, _ = strconv.ParseUint(s.NBFrames, 10, 64)
		case "audio":
			if s.CodecName != "aac" {
				return Media{}, ErrUnsupported
			}
		}
	}
	if videos == 0 {
		return Media{}, ErrInvalidMedia
	}
	if videos != 1 {
		return Media{}, ErrUnsupported
	}
	return Media{duration, frames}, nil
}

type Service struct {
	Config    config.VODConfig
	Logger    *slog.Logger
	Probe     Prober
	Now       func() time.Time
	store     atomic.Pointer[Store]
	cursorKey []byte
	sweepWake chan struct{}
}

func New(c config.VODConfig, db *Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{Config: c, Logger: logger, Probe: FFProbe{}, Now: func() time.Time { return time.Now().UTC() }}
	s.sweepWake = make(chan struct{}, 1)
	s.cursorKey = make([]byte, 32)
	if _, err := rand.Read(s.cursorKey); err != nil {
		panic(err)
	}
	s.store.Store(db)
	return s
}
func (s *Service) SetStore(db *Store) { s.store.Store(db) }
func (s *Service) Path(j Job, kind string) string {
	base := filepath.Join(s.Config.Root, j.ID.String())
	switch kind {
	case "original":
		return filepath.Join(base, "input.mp4")
	case "result":
		return filepath.Join(base, "output.mp4")
	case "work":
		return filepath.Join(base, "work")
	default:
		return filepath.Join(base, "output.mp4.part")
	}
}
func (s *Service) Finish(j *Job, state string) {
	now := s.Now()
	j.Status = state
	if j.filenameExpired(now) {
		j.OriginalFilename = nil
	}
	j.UpdatedAt = now
	if j.CompletedAt == nil {
		j.CompletedAt = &now
		deadline := now.Add(s.Config.OriginalRetention)
		j.OriginalDeleteAt = &deadline
	}
	if state == "done" {
		j.Progress = 100
		if j.ResultExpiresAt == nil {
			expiry := now.Add(s.Config.ResultRetention)
			j.ResultExpiresAt = &expiry
		}
	}
}

type View struct {
	ID               string     `json:"job_id"`
	OriginalFilename *string    `json:"original_filename,omitempty"`
	Status           string     `json:"status"`
	Progress         int        `json:"progress"`
	QueuePosition    *int       `json:"queue_position"`
	ETA              *int64     `json:"eta_sec"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	CompletedAt      *time.Time `json:"completed_at"`
	OriginalDeleteAt *time.Time `json:"original_delete_at"`
	ResultExpiresAt  *time.Time `json:"result_expires_at"`
	Failure          *Failure   `json:"failure"`
}
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Service) View(j Job, queue []Job) View {
	v := View{ID: j.ID.String(), Status: j.Status, Progress: j.Progress, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, CompletedAt: j.CompletedAt, OriginalDeleteAt: j.OriginalDeleteAt, ResultExpiresAt: j.ResultExpiresAt}
	if !j.filenameExpired(s.Now()) {
		v.OriginalFilename = j.OriginalFilename
	}
	if j.ResultExpiresAt != nil && !s.Now().Before(*j.ResultExpiresAt) && j.Status == "done" {
		v.Status = "expired"
	}
	if j.FailureCode != "" && (j.Status == "failed" || j.Status == "cancelled") {
		v.Failure = &Failure{Code: publicFailure(j.FailureCode).Code, Message: "Video processing failed."}
	}
	var speed float64
	var samples int
	for _, q := range queue {
		if q.ProcessingFPS > 0 && q.UpdatedAt.After(s.Now().Add(-10*time.Minute)) {
			speed += q.ProcessingFPS
			samples++
		}
	}
	if samples > 0 {
		speed /= float64(samples)
	}
	remaining := func(q Job) float64 {
		if q.FramesTotal == 0 {
			return -1
		}
		return float64(q.FramesTotal - min(q.FramesDone, q.FramesTotal))
	}
	if j.Status == "processing" && j.ProcessingFPS > 0 && remaining(j) >= 0 {
		eta := int64(math.Ceil(remaining(j) / j.ProcessingFPS))
		v.ETA = &eta
	}
	if j.Status == "queued" || j.Status == "paused" {
		position := 0
		work := float64(0)
		known := speed > 0
		active := false
		for _, q := range queue {
			if q.Status == "processing" && q.DeletedAt == nil {
				active = true
				r := remaining(q)
				if r < 0 {
					known = false
				} else {
					work += r
				}
			}
		}
		for _, q := range queue {
			if q.DeletedAt != nil || (q.Status != "queued" && q.Status != "paused") {
				continue
			}
			position++
			r := remaining(q)
			if r < 0 {
				known = false
			} else {
				work += r
			}
			if q.ID == j.ID {
				v.QueuePosition = &position
				break
			}
		}
		// If no worker is processing, live demand may make wait unbounded.
		if known && active {
			eta := int64(math.Ceil(work / speed))
			v.ETA = &eta
		}
	}
	return v
}

func (s *Service) WakeSweep() {
	select {
	case s.sweepWake <- struct{}{}:
	default:
	}
}
