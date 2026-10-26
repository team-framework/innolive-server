package vod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"inno-live-server/internal/auth"
	"inno-live-server/internal/config"
)

func testService(t *testing.T) *Service {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for VOD PostgreSQL integration")
	}
	admin, e := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if e != nil {
		t.Fatal(e)
	}
	schema := "vod_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if e = admin.Exec("CREATE SCHEMA " + schema).Error; e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, e := gorm.Open(postgres.Open(u.String()), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
		_ = admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error
		sqlAdmin, _ := admin.DB()
		_ = sqlAdmin.Close()
	})
	if e = AutoMigrate(context.Background(), db); e != nil {
		t.Fatal(e)
	}
	cfg := config.DefaultVOD()
	cfg.Enabled = true
	cfg.Root = t.TempDir()
	return New(cfg, NewStore(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
}
func createJob(t *testing.T, s *Service, state string) Job {
	t.Helper()
	now := s.Now()
	j := Job{ID: uuid.New(), UserID: uuid.New(), Status: state, CreatedAt: now, UpdatedAt: now, QueuedAt: now, DurationSec: 60, FramesTotal: 1800, Version: 1}
	if e := s.store.Load().Create(context.Background(), &j); e != nil {
		t.Fatal(e)
	}
	return j
}
func saveJob(t *testing.T, s *Service, j *Job) {
	t.Helper()
	if e := s.store.Load().Save(context.Background(), j); e != nil {
		t.Fatal(e)
	}
}
func writeFile(t *testing.T, path string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(path), 0750); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, []byte("video"), 0640); e != nil {
		t.Fatal(e)
	}
}

type activeUsers struct{}

func (activeUsers) UserStatus(context.Context, uuid.UUID) (auth.UserStatus, error) {
	return auth.UserStatusActive, nil
}

var testKey = []byte("vod-test-only-signing-key-32-bytes")

func requireUser() func(http.Handler) http.Handler {
	return auth.RequireUser(auth.NewTokenService(nil, auth.TokenConfig{AccessKey: testKey, Issuer: "vod-test", Audience: "vod-api"}), activeUsers{})
}
func token(t *testing.T, user uuid.UUID) string {
	t.Helper()
	now := time.Now()
	claims := auth.AccessClaims{SessionID: uuid.NewString(), TokenType: "access", RegisteredClaims: jwt.RegisteredClaims{Issuer: "vod-test", Audience: jwt.ClaimStrings{"vod-api"}, Subject: user.String(), ID: uuid.NewString(), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)), IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now.Add(-time.Second))}}
	v, e := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(testKey)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func handler(s *Service) http.Handler {
	mux := http.NewServeMux()
	s.Register(mux, requireUser())
	return mux
}
func request(t *testing.T, h http.Handler, method, path string, user uuid.UUID, body io.Reader, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, body)
	if user != uuid.Nil {
		r.Header.Set("Authorization", "Bearer "+token(t, user))
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type fakeProbe struct {
	duration float64
	err      error
}

func (p fakeProbe) Probe(context.Context, string) (Media, error) {
	return Media{p.duration, 1800}, p.err
}
func uploadBody(t *testing.T, n, parts int) (*bytes.Buffer, string) {
	t.Helper()
	b := new(bytes.Buffer)
	m := multipart.NewWriter(b)
	for i := 0; i < parts; i++ {
		headers := textproto.MIMEHeader{}
		headers.Set("Content-Disposition", `form-data; name="file"; filename="../../private.mp4"`)
		headers.Set("Content-Type", "video/mp4")
		p, e := m.CreatePart(headers)
		if e != nil {
			t.Fatal(e)
		}
		_, _ = p.Write(bytes.Repeat([]byte("x"), n))
	}
	_ = m.Close()
	return b, m.FormDataContentType()
}

func TestAPIOwnershipAndAuthentication(t *testing.T) {
	s := testService(t)
	j := createJob(t, s, "queued")
	h := handler(s)
	for _, method := range []string{"GET", "DELETE"} {
		for _, path := range []string{"/vod/jobs/" + j.ID.String(), "/vod/jobs/" + j.ID.String() + "/result"} {
			if method == "DELETE" && strings.HasSuffix(path, "result") {
				continue
			}
			w := request(t, h, method, path, uuid.New(), nil, "")
			if w.Code != 404 {
				t.Fatalf("foreign %s %s = %d", method, path, w.Code)
			}
		}
	}
	for _, path := range []string{"/vod/jobs", "/vod/jobs/bad-id", "/vod/jobs/" + j.ID.String() + "/result"} {
		if w := request(t, h, "GET", path, uuid.Nil, nil, ""); w.Code != 401 {
			t.Fatalf("unauthenticated = %d", w.Code)
		} else if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("cache header missing")
		}
	}
	if w := request(t, h, "GET", "/vod/jobs/bad-id", j.UserID, nil, ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := request(t, h, "GET", "/vod/jobs", uuid.New(), nil, ""); !strings.Contains(w.Body.String(), `"jobs":[]`) {
		t.Fatal(w.Body.String())
	}
	if w := request(t, h, "GET", "/vod/jobs/"+j.ID.String(), j.UserID, nil, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
}
func TestUploadLimitsAndValidation(t *testing.T) {
	cases := []struct {
		name     string
		n, parts int
		duration float64
		err      error
		code     int
	}{{"accepted", 10, 1, 60, nil, 201}, {"byte_boundary", 32, 1, 3600, nil, 201}, {"bytes", 33, 1, 60, nil, 413}, {"duration", 10, 1, 3601, nil, 422}, {"fractional_duration", 10, 1, 3600.1, nil, 422}, {"empty", 0, 1, 60, nil, 400}, {"duplicates", 10, 2, 60, nil, 400}, {"unsupported", 10, 1, 60, ErrUnsupported, 415}, {"corrupt", 10, 1, 0, ErrInvalidMedia, 422}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testService(t)
			s.Config.MaxUploadBytes = 32
			s.Probe = fakeProbe{tc.duration, tc.err}
			b, ct := uploadBody(t, tc.n, tc.parts)
			r := httptest.NewRequest("POST", "/vod/jobs", b)
			r.ContentLength = -1
			r.Header.Set("Content-Type", ct)
			r.Header.Set("Authorization", "Bearer "+token(t, uuid.New()))
			w := httptest.NewRecorder()
			handler(s).ServeHTTP(w, r)
			if w.Code != tc.code {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
			if tc.code == 413 && !strings.Contains(w.Body.String(), `"code":"upload_too_large"`) {
				t.Fatal(w.Body.String())
			}
			if (tc.name == "duration" || tc.name == "fractional_duration") && !strings.Contains(w.Body.String(), `"code":"duration_exceeded"`) {
				t.Fatal(w.Body.String())
			}
			jobs, e := s.store.Load().All(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			files, _ := os.ReadDir(s.Config.Root)
			if tc.code == 201 {
				if len(jobs) != 1 || len(files) != 1 {
					t.Fatal("missing persisted upload")
				}
			} else if len(jobs) != 0 || len(files) != 0 {
				t.Fatal("failed upload retained data")
			}
		})
	}
}
func TestAPIDeletePreventsLateWorkerResurrection(t *testing.T) {
	s := testService(t)
	j := createJob(t, s, "processing")
	stale := j
	h := handler(s)
	if w := request(t, h, "DELETE", "/vod/jobs/"+j.ID.String(), j.UserID, nil, ""); w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	stale.Status = "done"
	if e := s.store.Load().Save(context.Background(), &stale); !errors.Is(e, ErrConflict) {
		t.Fatal("late update was accepted", e)
	}
	for _, method := range []string{"GET", "DELETE"} {
		if w := request(t, h, method, "/vod/jobs/"+j.ID.String(), j.UserID, nil, ""); w.Code != 404 {
			t.Fatal(w.Code)
		}
	}
	all, _ := s.store.Load().All(context.Background())
	if all[0].Status != "cancelled" || all[0].OriginalDeleteAt == nil {
		t.Fatal("lost cancellation obligation")
	}
}
func TestResultAndExpiryBoundary(t *testing.T) {
	s := testService(t)
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	j := createJob(t, s, "processing")
	s.Finish(&j, "done")
	saveJob(t, s, &j)
	writeFile(t, s.Path(j, "result"))
	h := handler(s)
	path := "/vod/jobs/" + j.ID.String() + "/result"
	now = j.ResultExpiresAt.Add(-time.Nanosecond)
	w := request(t, h, "GET", path, j.UserID, nil, "")
	if w.Code != 200 || w.Body.String() != "video" || w.Header().Get("Accept-Ranges") != "none" {
		t.Fatal(w.Code, w.Body.String())
	}
	now = *j.ResultExpiresAt
	w = request(t, h, "GET", path, j.UserID, nil, "")
	if w.Code != 410 {
		t.Fatal(w.Code)
	}
}
func TestCursorOwnerTamperingAndPagination(t *testing.T) {
	s := testService(t)
	j := createJob(t, s, "queued")
	other := createJob(t, s, "failed")
	other.UserID = j.UserID
	saveJob(t, s, &other)
	h := handler(s)
	w := request(t, h, "GET", "/vod/jobs?limit=1", j.UserID, nil, "")
	var page struct {
		Jobs []View
		Next *string `json:"next_cursor"`
	}
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Next == nil || len(page.Jobs) != 1 {
		t.Fatal(w.Body.String())
	}
	for _, tc := range []struct {
		user   uuid.UUID
		cursor string
	}{{uuid.New(), *page.Next}, {j.UserID, *page.Next + "tampered"}} {
		w = request(t, h, "GET", "/vod/jobs?cursor="+url.QueryEscape(tc.cursor), tc.user, nil, "")
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	w = request(t, h, "GET", "/vod/jobs?cursor="+url.QueryEscape(*page.Next), j.UserID, nil, "")
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 || page.Next != nil {
		t.Fatal(w.Body.String())
	}
}
func TestRetention24HoursAndSevenDays(t *testing.T) {
	for _, state := range []string{"done", "failed", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			s := testService(t)
			now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
			s.Now = func() time.Time { return now }
			j := createJob(t, s, "processing")
			s.Finish(&j, state)
			saveJob(t, s, &j)
			for _, kind := range []string{"original", "result", "partial"} {
				writeFile(t, s.Path(j, kind))
			}
			writeFile(t, filepath.Join(s.Path(j, "work"), "checkpoint.json"))
			if j.OriginalDeleteAt.Sub(*j.CompletedAt) != 24*time.Hour {
				t.Fatal("original deadline")
			}
			now = now.Add(24*time.Hour - time.Nanosecond)
			if e := s.Sweep(context.Background()); e != nil {
				t.Fatal(e)
			}
			if _, e := os.Stat(s.Path(j, "original")); !os.IsNotExist(e) {
				t.Fatal("original retained")
			}
			if state == "done" {
				now = j.ResultExpiresAt.Add(-time.Nanosecond)
				_ = s.Sweep(context.Background())
				if _, e := os.Stat(s.Path(j, "result")); e != nil {
					t.Fatal("early result deletion")
				}
				now = *j.ResultExpiresAt
				if e := s.Sweep(context.Background()); e != nil {
					t.Fatal(e)
				}
				got, _ := s.store.Load().Get(context.Background(), j.ID, j.UserID)
				if got.Status != "expired" {
					t.Fatal(got.Status)
				}
			}
			if _, e := os.Stat(s.Path(j, "result")); !os.IsNotExist(e) {
				t.Fatal("result retained")
			}
			_ = s.Sweep(context.Background())
			var count int64
			s.store.Load().DB.Model(&DeletionLog{}).Where("job_id = ?", j.ID).Count(&count)
			if count != 4 {
				t.Fatalf("audit count %d", count)
			}
		})
	}
}
func TestSweeperDoesNotDeleteUnacknowledgedWorkerFiles(t *testing.T) {
	s := testService(t)
	j := createJob(t, s, "cancelled")
	j.WorkerTarget = "unreachable"
	saveJob(t, s, &j)
	writeFile(t, s.Path(j, "original"))
	if e := s.Sweep(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(s.Path(j, "original")); e != nil {
		t.Fatal("worker input deleted before cancel ack")
	}
}
func TestDisabledCreatesNoRoutesOrWorkers(t *testing.T) {
	c := config.DefaultVOD()
	s := New(c, nil, nil)
	h := handler(s)
	if w := request(t, h, "GET", "/vod/jobs", uuid.Nil, nil, ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	NewEngine(s, nil, nil, nil).Run(context.Background())
	s.RunSweeper(context.Background())
}

func TestStorageSymlinksCannotEscapeRoot(t *testing.T) {
	s := testService(t)
	j := createJob(t, s, "processing")
	s.Finish(&j, "done")
	saveJob(t, s, &j)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "input.mp4"))
	writeFile(t, filepath.Join(outside, "output.mp4"))
	if err := os.Symlink(outside, filepath.Join(s.Config.Root, j.ID.String())); err != nil {
		t.Fatal(err)
	}
	if w := request(t, handler(s), "GET", "/vod/jobs/"+j.ID.String()+"/result", j.UserID, nil, ""); w.Code != 503 {
		t.Fatal("escaped download", w.Code)
	}
	if err := s.Sweep(context.Background()); err == nil {
		t.Fatal("escaped purge accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "input.mp4")); err != nil {
		t.Fatal("outside data removed")
	}
}
func TestSweeperCleansAbandonedUpload(t *testing.T) {
	s := testService(t)
	j := Job{ID: uuid.New()}
	writeFile(t, s.Path(j, "original"))
	dir := filepath.Dir(s.Path(j, "original"))
	old := s.Now().Add(-s.Config.UploadTimeout - 2*s.Config.SweepInterval)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("abandoned upload retained")
	}
}

func TestFFProbeRealMediaValidation(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	for _, codec := range []string{"libx264", "mpeg4"} {
		t.Run(codec, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sample.mp4")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=c=black:s=64x64:r=5", "-t", "1", "-c:v", codec, "-threads", "1", "-pix_fmt", "yuv420p", path)
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v %s", err, b)
			}
			m, err := (FFProbe{}).Probe(ctx, path)
			if codec == "libx264" {
				if err != nil || m.Duration != 1 || m.Frames != 5 {
					t.Fatal(m, err)
				}
			} else if !errors.Is(err, ErrUnsupported) {
				t.Fatal("unsupported codec accepted", err)
			}
		})
	}
}

func TestCancellationDeadlinePurgesEvenWhenWorkerUnavailable(t *testing.T) {
	s := testService(t)
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	j := createJob(t, s, "processing")
	j.WorkerTarget = "unreachable"
	s.Finish(&j, "cancelled")
	saveJob(t, s, &j)
	writeFile(t, s.Path(j, "original"))
	now = j.OriginalDeleteAt.Add(-time.Nanosecond)
	if err := s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path(j, "original")); err != nil {
		t.Fatal("removed before cancellation acknowledgement or deadline")
	}
	now = *j.OriginalDeleteAt
	if err := s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path(j, "original")); !os.IsNotExist(err) {
		t.Fatal("retention extended by unavailable worker")
	}
}

func TestActiveDownloadStopsOnDeleteOrExpiry(t *testing.T) {
	for _, cause := range []string{"delete", "expiry"} {
		t.Run(cause, func(t *testing.T) {
			s := testService(t)
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			s.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			j := createJob(t, s, "processing")
			s.Finish(&j, "done")
			saveJob(t, s, &j)
			writeFile(t, s.Path(j, "result"))
			const size = 64 << 20
			if err := os.Truncate(s.Path(j, "result"), size); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(handler(s))
			defer server.Close()
			client := &http.Client{Timeout: 5 * time.Second}
			req, _ := http.NewRequest("GET", server.URL+"/vod/jobs/"+j.ID.String()+"/result", nil)
			req.Header.Set("Authorization", "Bearer "+token(t, j.UserID))
			response, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatal(response.StatusCode)
			}
			if cause == "delete" {
				req, _ := http.NewRequest("DELETE", server.URL+"/vod/jobs/"+j.ID.String(), nil)
				req.Header.Set("Authorization", "Bearer "+token(t, j.UserID))
				deleted, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = deleted.Body.Close()
				if deleted.StatusCode != 204 {
					t.Fatal(deleted.StatusCode)
				}
			} else {
				clock.Store(j.ResultExpiresAt.UnixNano())
			}
			n, err := io.Copy(io.Discard, response.Body)
			if err == nil || n >= size {
				t.Fatalf("download continued after %s: bytes=%d err=%v", cause, n, err)
			}
		})
	}
}
