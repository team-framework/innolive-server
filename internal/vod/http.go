package vod

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"inno-live-server/internal/auth"
)

// Register creates no routes when disabled. Authentication wraps every method.
func (s *Service) Register(mux *http.ServeMux, requireUser func(http.Handler) http.Handler) {
	if !s.Config.Enabled {
		return
	}
	for pattern, handler := range map[string]http.HandlerFunc{"POST /vod/jobs": s.upload, "GET /vod/jobs": s.list, "GET /vod/jobs/{job_id}": s.get, "DELETE /vod/jobs/{job_id}": s.delete, "GET /vod/jobs/{job_id}/result": s.result} {
		authenticated := requireUser(handler)
		mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			authenticated.ServeHTTP(w, r)
		}))
	}
}
// acceptedUploadContentType allows landing/browser MIME values for MP4 and QuickTime
// (.mov) uploads. Empty Content-Type is treated like application/octet-stream so
// browsers that omit a type for .mov still reach the ffprobe byte check.
func acceptedUploadContentType(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return false
	}
	switch strings.ToLower(mediaType) {
	case "video/mp4", "video/quicktime", "video/x-quicktime", "application/octet-stream":
		return true
	default:
		return false
	}
}

func fail(w http.ResponseWriter, code int, key string, details ...map[string]any) {
	body := map[string]any{"code": key, "message": strings.ReplaceAll(key, "_", " ")}
	if len(details) > 0 {
		body["details"] = details[0]
	}
	reply(w, code, map[string]any{"error": body})
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Service) access(w http.ResponseWriter, r *http.Request) (*Store, uuid.UUID, bool) {
	user, ok := auth.UserIDFromContext(r.Context())
	if !ok || user == uuid.Nil {
		fail(w, 401, "unauthorized")
		return nil, user, false
	}
	db := s.store.Load()
	if db == nil {
		fail(w, 503, "service_unavailable")
		return nil, user, false
	}
	return db, user, true
}
func (s *Service) owned(w http.ResponseWriter, r *http.Request) (*Store, Job, bool) {
	db, user, ok := s.access(w, r)
	if !ok {
		return nil, Job{}, false
	}
	id, e := uuid.Parse(r.PathValue("job_id"))
	if e != nil {
		fail(w, 400, "bad_request")
		return nil, Job{}, false
	}
	j, e := db.Get(r.Context(), id, user)
	if errors.Is(e, gorm.ErrRecordNotFound) {
		fail(w, 404, "not_found")
		return nil, j, false
	}
	if e != nil {
		fail(w, 503, "service_unavailable")
		return nil, j, false
	}
	return db, j, true
}
func (s *Service) upload(w http.ResponseWriter, r *http.Request) {
	db, user, ok := s.access(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.Config.UploadTimeout)
	defer cancel()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(s.Config.UploadTimeout))
	defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
	maxBytes := s.Config.MaxUploadBytes
	tooLarge := func() {
		fail(w, 413, "upload_too_large", map[string]any{"max_upload_bytes": maxBytes, "max_request_bytes": maxBytes + 1048576})
	}
	if r.ContentLength > maxBytes+1048576 {
		tooLarge()
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1048576)
	reader, e := r.MultipartReader()
	if e != nil {
		fail(w, 400, "bad_request")
		return
	}
	part, e := reader.NextPart()
	if e != nil {
		var capErr *http.MaxBytesError
		if errors.As(e, &capErr) {
			tooLarge()
		} else {
			fail(w, 400, "bad_request")
		}
		return
	}
	// Read the raw parameter: Part.FileName applies OS-specific path cleaning
	// and can turn a trailing separator into a nonempty parent directory name.
	_, disposition, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	filename, hasFilename := disposition["filename"]
	if err != nil || part.FormName() != "file" || !hasFilename {
		fail(w, 400, "bad_request")
		return
	}
	if !acceptedUploadContentType(part.Header.Get("Content-Type")) {
		fail(w, 415, "unsupported_media_type")
		return
	}
	now := s.Now()
	j := Job{ID: uuid.New(), UserID: user, Status: "uploaded", CreatedAt: now, UpdatedAt: now, QueuedAt: now, Version: 1}
	j.OriginalFilename = sanitizeOriginalFilename(filename)
	path := s.Path(j, "original")
	dir := filepath.Dir(path)
	if e = mkdirShared(dir); e != nil {
		fail(w, 503, "service_unavailable")
		return
	}
	accepted := false
	defer func() {
		if !accepted {
			if e := os.RemoveAll(dir); e != nil {
				s.Logger.Error("vod upload cleanup failed", "job_id", j.ID, "error", e)
			}
		}
	}()
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0660)
	if e != nil {
		fail(w, 503, "service_unavailable")
		return
	}
	// Creation modes are filtered by umask; the worker must retain group write.
	if e = f.Chmod(0660); e != nil {
		_ = f.Close()
		fail(w, 503, "service_unavailable")
		return
	}
	n, e := copyUpload(f, part, maxBytes)
	var syncErr error
	if e == nil {
		syncErr = f.Sync()
	}
	closeErr := f.Close()
	var capErr *http.MaxBytesError
	if errors.Is(e, errUploadTooLarge) || errors.As(e, &capErr) {
		tooLarge()
		return
	}
	if e != nil {
		fail(w, 400, "bad_request")
		return
	}
	if syncErr != nil || closeErr != nil {
		fail(w, 503, "service_unavailable")
		return
	}
	if n == 0 {
		fail(w, 400, "bad_request")
		return
	}
	if _, e = reader.NextPart(); e != io.EOF {
		if errors.As(e, &capErr) {
			tooLarge()
		} else {
			fail(w, 400, "bad_request")
		}
		return
	}
	// Multipart epilogues count toward the request bound as well.
	if _, e = io.Copy(io.Discard, r.Body); e != nil {
		if errors.As(e, &capErr) {
			tooLarge()
		} else {
			fail(w, 400, "bad_request")
		}
		return
	}
	media, e := s.Probe.Probe(ctx, path)
	if errors.Is(e, ErrUnsupported) {
		fail(w, 415, "unsupported_media_type")
		return
	}
	if e != nil || media.Duration <= 0 {
		fail(w, 422, "invalid_media")
		return
	}
	if media.Duration > s.Config.MaxDuration.Seconds() {
		fail(w, 422, "duration_exceeded", map[string]any{"max_duration_sec": s.Config.MaxDuration.Seconds()})
		return
	}
	j.DurationSec = media.Duration
	j.FramesTotal = media.Frames
	d, e := os.Open(dir)
	if e == nil {
		e = d.Sync()
		_ = d.Close()
	}
	if e != nil {
		fail(w, 503, "service_unavailable")
		return
	}
	if e = db.Create(ctx, &j); e != nil {
		fail(w, 503, "service_unavailable")
		return
	}
	accepted = true
	w.Header().Set("Location", "/vod/jobs/"+j.ID.String())
	reply(w, 201, s.View(j, nil))
}
func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	db, j, ok := s.owned(w, r)
	if !ok {
		return
	}
	queue, e := db.All(r.Context())
	if e != nil {
		fail(w, 503, "service_unavailable")
		return
	}
	reply(w, 200, s.View(j, queue))
}

// Cursors use a process-local random key; restart invalidates pagination tokens.
// Cursor MAC is scoped to a user and includes the initial upper and last keys.
type cursor struct {
	User  string
	Upper time.Time
	At    time.Time
	ID    string
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	db, user, ok := s.access(w, r)
	if !ok {
		return
	}
	limit := 20
	if raw, exists := r.URL.Query()["limit"]; exists {
		var e error
		limit, e = strconv.Atoi(raw[0])
		if e != nil || limit < 1 || limit > 100 {
			fail(w, 400, "bad_request")
			return
		}
	}
	c := cursor{User: user.String(), Upper: s.Now()}
	if raw, exists := r.URL.Query()["cursor"]; exists {
		parts := strings.Split(raw[0], ".")
		if len(parts) != 2 {
			fail(w, 400, "bad_request")
			return
		}
		b, e := base64.RawURLEncoding.DecodeString(parts[0])
		mac, e2 := base64.RawURLEncoding.DecodeString(parts[1])
		if e != nil || e2 != nil || !hmac.Equal(mac, s.cursorMAC(b)) || json.Unmarshal(b, &c) != nil || c.User != user.String() {
			fail(w, 400, "bad_request")
			return
		}
	}
	queue, e := db.All(r.Context())
	if e != nil {
		fail(w, 503, "service_unavailable")
		return
	}
	jobs := make([]Job, 0)
	for _, j := range queue {
		if j.UserID != user || j.DeletedAt != nil || j.CreatedAt.After(c.Upper) {
			continue
		}
		if !c.At.IsZero() && (j.CreatedAt.After(c.At) || (j.CreatedAt.Equal(c.At) && j.ID.String() >= c.ID)) {
			continue
		}
		jobs = append(jobs, j)
	}
	sort.Slice(jobs, func(i, k int) bool {
		if jobs[i].CreatedAt.Equal(jobs[k].CreatedAt) {
			return jobs[i].ID.String() > jobs[k].ID.String()
		}
		return jobs[i].CreatedAt.After(jobs[k].CreatedAt)
	})
	var next *string
	if len(jobs) > limit {
		jobs = jobs[:limit]
		last := jobs[len(jobs)-1]
		c.At = last.CreatedAt
		c.ID = last.ID.String()
		b, _ := json.Marshal(c)
		v := base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(s.cursorMAC(b))
		next = &v
	}
	views := make([]View, 0, len(jobs))
	for _, j := range jobs {
		views = append(views, s.View(j, queue))
	}
	reply(w, 200, map[string]any{"jobs": views, "next_cursor": next})
}
func (s *Service) cursorMAC(b []byte) []byte {
	h := hmac.New(sha256.New, s.cursorKey)
	h.Write(b)
	return h.Sum(nil)
}
func (s *Service) SetCursorKey(key []byte) { s.cursorKey = append([]byte{}, key...) }
func (s *Service) delete(w http.ResponseWriter, r *http.Request) {
	db, j, ok := s.owned(w, r)
	if !ok {
		return
	}
	for attempt := 0; attempt < 5; attempt++ {
		now := s.Now()
		j.DeletedAt = &now
		s.Finish(&j, "cancelled")
		deadline := now.Add(s.Config.OriginalRetention)
		if j.OriginalDeleteAt == nil || deadline.Before(*j.OriginalDeleteAt) {
			j.OriginalDeleteAt = &deadline
		}
		e := db.Save(r.Context(), &j)
		if e == nil {
			s.WakeSweep()
			w.WriteHeader(204)
			return
		}
		if !errors.Is(e, ErrConflict) {
			fail(w, 503, "service_unavailable")
			return
		}
		j, e = db.Get(r.Context(), j.ID, j.UserID)
		if e != nil {
			fail(w, 404, "not_found")
			return
		}
	}
	fail(w, 503, "service_unavailable")
}
func (s *Service) result(w http.ResponseWriter, r *http.Request) {
	db, j, ok := s.owned(w, r)
	if !ok {
		return
	}
	if j.Status == "expired" || (j.ResultExpiresAt != nil && !s.Now().Before(*j.ResultExpiresAt)) {
		fail(w, 410, "result_expired")
		return
	}
	if j.Status == "failed" || j.Status == "cancelled" {
		mapped := publicFailure(j.FailureCode)
		if j.Status == "cancelled" {
			mapped = mapWorkerFailure("job:CANCELLED")
		}
		fail(w, mapped.HTTP, mapped.Code)
		return
	}
	if j.Status != "done" || j.ResultExpiresAt == nil {
		fail(w, 409, "result_not_ready")
		return
	}
	root, e := os.OpenRoot(s.Config.Root)
	if e != nil {
		fail(w, 503, "service_unavailable")
		return
	}
	defer root.Close()
	relative, e := filepath.Rel(s.Config.Root, s.Path(j, "result"))
	if e != nil {
		fail(w, 503, "service_unavailable")
		return
	}
	f, e := root.Open(relative)
	if e != nil {
		fail(w, 503, "service_unavailable")
		return
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		fail(w, 503, "service_unavailable")
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(j.ResultExpiresAt.Sub(s.Now())))
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	ctx, cancel := context.WithCancel(r.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, e := db.Get(ctx, j.ID, j.UserID)
				if e != nil || current.Status != "done" || !s.Now().Before(*j.ResultExpiresAt) {
					_ = rc.SetWriteDeadline(time.Now())
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"vod-%s.mp4\"", j.ID))
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Accept-Ranges", "none")
	w.WriteHeader(200)
	buf := make([]byte, 64*1024)
	for {
		if ctx.Err() != nil || !s.Now().Before(*j.ResultExpiresAt) {
			panic(http.ErrAbortHandler)
		}
		// Re-authorize each chunk across server slots. The watcher above also
		// interrupts a write blocked on a slow client; polling alone could let
		// a fast transfer finish after DELETE before the next watcher tick.
		current, checkErr := db.Get(ctx, j.ID, j.UserID)
		if checkErr != nil || current.Status != "done" {
			panic(http.ErrAbortHandler)
		}
		n, e := f.Read(buf)
		if n > 0 {
			if _, we := w.Write(buf[:n]); we != nil {
				panic(http.ErrAbortHandler)
			}
		}
		if e == io.EOF {
			return
		}
		if e != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

// Read at most one extra byte to detect overflow, without writing it or draining
// the remainder. Multipart parsing may buffer a bounded amount ahead of us.
var errUploadTooLarge = errors.New("upload too large")

func copyUpload(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	n, err := io.Copy(dst, io.LimitReader(src, limit))
	if err != nil {
		return n, err
	}
	var extra [1]byte
	count, err := io.ReadFull(src, extra[:])
	if count != 0 {
		return n, errUploadTooLarge
	}
	if err == io.EOF {
		err = nil
	}
	return n, err
}
