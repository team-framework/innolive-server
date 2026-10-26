package vod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"inno-live-server/internal/auth"
)

func TestSanitizeOriginalFilename(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"plain", "video.mp4", "video.mp4"},
		{"path", "/private/../video.mp4", "video.mp4"},
		{"backslash", `C:\private\video.mp4`, "video.mp4"},
		{"mixed", `/private\folder/video.mp4`, "video.mp4"},
		{"controls", " \x00vi\n\r\tdeo\x7f\u0085.mp4\x01 ", "video.mp4"},
		{"spaces", "\u2003 영상 이름.mp4 \u2003", "영상 이름.mp4"},
		{"invalid_utf8", "video\xff\xfe.mp4", "video\uFFFD.mp4"},
		{"ascii_limit", strings.Repeat("a", 256), strings.Repeat("a", 255)},
		{"exact_limit", strings.Repeat("a", 255), strings.Repeat("a", 255)},
		{"multibyte", strings.Repeat("가", 86), strings.Repeat("가", 85)},
		{"split_multibyte", strings.Repeat("a", 254) + "가.mp4", strings.Repeat("a", 254)},
		{"split_emoji", strings.Repeat("a", 253) + "😀.mp4", strings.Repeat("a", 253)},
		{"trim_after_cut", strings.Repeat("a", 254) + " b", strings.Repeat("a", 254)},
		{"empty", "", ""}, {"only_controls", "\x00\n\t ", ""},
		{"dot", "dir/ . ", ""}, {"dotdot", `dir\..`, ""},
		{"trailing_slash", "dir/", ""}, {"trailing_backslash", `dir\`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeOriginalFilename(tc.input)
			if tc.want == "" {
				if got != nil {
					t.Fatal("empty filename must be nil")
				}
				return
			}
			if got == nil || *got != tc.want || len(*got) > 255 || !utf8.ValidString(*got) {
				t.Fatalf("unexpected sanitized filename: %v", got)
			}
		})
	}
}

func filenameUpload(t *testing.T, filename string) (*bytes.Buffer, string) {
	t.Helper()
	b := new(bytes.Buffer)
	m := multipart.NewWriter(b)
	headers := textproto.MIMEHeader{}
	headers.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": filename}))
	headers.Set("Content-Type", "video/mp4")
	p, err := m.CreatePart(headers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Write([]byte("video")); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	return b, m.FormDataContentType()
}

func assertFilenameJSON(t *testing.T, body []byte, want string) {
	t.Helper()
	var v map[string]json.RawMessage
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	value, exists := v["original_filename"]
	if want == "" {
		if exists {
			t.Fatal("filename must be omitted, not null or empty")
		}
		return
	}
	var got string
	if !exists || json.Unmarshal(value, &got) != nil || got != want {
		t.Fatal("unexpected response filename")
	}
}

func assertFilenameNULL(t *testing.T, s *Service, j Job) {
	t.Helper()
	var erased bool
	if err := s.store.Load().DB.Raw("SELECT original_filename IS NULL FROM vod_jobs WHERE id = ?", j.ID).Scan(&erased).Error; err != nil {
		t.Fatal(err)
	}
	if !erased {
		t.Fatal("filename was not SQL NULL")
	}
}

func TestOriginalFilenameAPI(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"unix", "../../private.mp4", "private.mp4"},
		{"windows", `C:\private\video.mp4`, "video.mp4"},
		{"controls", "\x00 private\n.mp4 \x7f", "private.mp4"},
		{"unicode", "영상.mp4", "영상.mp4"},
		{"invalid_utf8", "video\xff.mp4", "video\uFFFD.mp4"},
		{"long", strings.Repeat("가", 86), strings.Repeat("가", 85)},
		{"empty", "", ""}, {"spaces", "  ", ""}, {"dot", ".", ""},
		{"dotdot", "..", ""}, {"trailing_separator", "private/", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testService(t)
			s.Probe = fakeProbe{duration: 60}
			h := handler(s)
			user := uuid.New()
			b, ct := filenameUpload(t, tc.input)
			w := request(t, h, "POST", "/vod/jobs", user, b, ct)
			if w.Code != 201 {
				t.Fatalf("upload status %d", w.Code)
			}
			assertFilenameJSON(t, w.Body.Bytes(), tc.want)
			jobs, err := s.store.Load().All(context.Background())
			if err != nil || len(jobs) != 1 {
				t.Fatal("missing job", err)
			}
			j := jobs[0]
			if tc.want == "" {
				assertFilenameNULL(t, s, j)
			} else if j.OriginalFilename == nil || *j.OriginalFilename != tc.want {
				t.Fatal("filename not persisted")
			}
			w = request(t, h, "GET", "/vod/jobs/"+j.ID.String(), user, nil, "")
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
			assertFilenameJSON(t, w.Body.Bytes(), tc.want)
			w = request(t, h, "GET", "/vod/jobs", user, nil, "")
			var page struct{ Jobs []json.RawMessage }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
				t.Fatal("invalid list")
			}
			assertFilenameJSON(t, page.Jobs[0], tc.want)
		})
	}
}

func TestOriginalFilenameRetentionAndLogs(t *testing.T) {
	for _, retention := range []time.Duration{7 * 24 * time.Hour, time.Hour} {
		t.Run(retention.String(), func(t *testing.T) {
			s := testService(t)
			s.Config.ResultRetention = retention
			now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
			s.Now = func() time.Time { return now }
			var logs bytes.Buffer
			s.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			// NewStore must suppress SQL logging even if its caller enables it.
			db := s.store.Load().DB.Session(&gorm.Session{Logger: gormlogger.New(log.New(&logs, "", 0), gormlogger.Config{LogLevel: gormlogger.Info})})
			s.SetStore(NewStore(db))
			s.Probe = fakeProbe{duration: 60}
			const private = "private-customer-identity.mp4"
			b, ct := filenameUpload(t, "private-directory/"+private)
			h := handler(s)
			user := uuid.New()
			if w := request(t, h, "POST", "/vod/jobs", user, b, ct); w.Code != 201 {
				t.Fatal(w.Code)
			}
			jobs, err := s.store.Load().All(context.Background())
			if err != nil || len(jobs) != 1 {
				t.Fatal("missing job", err)
			}
			j := jobs[0]
			s.Finish(&j, "done")
			saveJob(t, s, &j)
			writeFile(t, s.Path(j, "result"))
			if j.ResultExpiresAt.Sub(*j.CompletedAt) != retention {
				t.Fatal("wrong retention")
			}
			now = j.ResultExpiresAt.Add(-time.Nanosecond)
			if err = s.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			stored, err := s.store.Load().Get(context.Background(), j.ID, j.UserID)
			if err != nil || stored.OriginalFilename == nil {
				t.Fatal("early filename erasure", err)
			}
			w := request(t, h, "GET", "/vod/jobs/"+j.ID.String()+"/result", user, nil, "")
			if w.Code != 200 || w.Header().Get("Content-Disposition") != fmt.Sprintf(`attachment; filename="vod-%s.mp4"`, j.ID) {
				t.Fatal("download filename changed")
			}
			now = *j.ResultExpiresAt
			w = request(t, h, "GET", "/vod/jobs/"+j.ID.String(), user, nil, "")
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
			assertFilenameJSON(t, w.Body.Bytes(), "")
			stale := stored
			if err = s.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertFilenameNULL(t, s, j)
			if _, err = os.Stat(s.Path(j, "result")); !os.IsNotExist(err) {
				t.Fatal("result retained")
			}
			if err = s.store.Load().Save(context.Background(), &stale); !errors.Is(err, ErrConflict) {
				t.Fatal("stale write restored filename", err)
			}
			if err = s.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			var audit []string
			if err = s.store.Load().DB.Raw("SELECT row_to_json(l)::text FROM vod_deletion_logs l WHERE job_id = ?", j.ID).Scan(&audit).Error; err != nil {
				t.Fatal(err)
			}
			if len(audit) != 4 || !strings.Contains(logs.String(), "vod_file_destroyed") {
				t.Fatal("missing deletion evidence")
			}
			for _, evidence := range []string{logs.String(), strings.Join(audit, "\n")} {
				if strings.Contains(evidence, private) || strings.Contains(evidence, "private-directory") || strings.Contains(evidence, "original_filename") {
					t.Fatal("filename leaked into logs")
				}
			}
		})
	}
}

func TestOriginalFilenameErasedOnTermination(t *testing.T) {
	for _, state := range []string{"delete", "cancelled", "failed"} {
		t.Run(state, func(t *testing.T) {
			s := testService(t)
			j := createJob(t, s, "processing")
			j.OriginalFilename = sanitizeOriginalFilename("private.mp4")
			j.WorkerTarget = "unreachable"
			saveJob(t, s, &j)
			stale := j
			if state == "delete" {
				if w := request(t, handler(s), "DELETE", "/vod/jobs/"+j.ID.String(), j.UserID, nil, ""); w.Code != 204 {
					t.Fatal(w.Code)
				}
			} else {
				s.Finish(&j, state)
				saveJob(t, s, &j)
			}
			assertFilenameNULL(t, s, j)
			if err := s.store.Load().Save(context.Background(), &stale); !errors.Is(err, ErrConflict) {
				t.Fatal("stale write accepted", err)
			}
		})
	}
}

func TestOriginalFilenameErasedDespitePurgeFailure(t *testing.T) {
	s := testService(t)
	j := createJob(t, s, "processing")
	j.OriginalFilename = sanitizeOriginalFilename("private.mp4")
	s.Finish(&j, "done")
	saveJob(t, s, &j)
	s.Now = func() time.Time { return *j.ResultExpiresAt }
	// An inaccessible root prevents result deletion, but cannot extend metadata retention.
	s.Config.Root = s.Config.Root + "/missing"
	if err := s.Sweep(context.Background()); err == nil {
		t.Fatal("expected purge failure")
	}
	assertFilenameNULL(t, s, j)
}

func TestOriginalFilenameAccountWithdrawal(t *testing.T) {
	s := testService(t)
	db := s.store.Load().DB
	if err := db.AutoMigrate(&auth.User{}, &auth.EmailAccount{}, &auth.OAuthAccount{}, &auth.RefreshSession{}, &auth.StreamingAccount{}); err != nil {
		t.Fatal(err)
	}
	j := createJob(t, s, "processing")
	j.OriginalFilename = sanitizeOriginalFilename("private.mp4")
	saveJob(t, s, &j)
	other := createJob(t, s, "queued")
	other.OriginalFilename = sanitizeOriginalFilename("other.mp4")
	saveJob(t, s, &other)
	if err := db.Create(&auth.User{ID: j.UserID, Status: auth.UserStatusActive}).Error; err != nil {
		t.Fatal(err)
	}
	// A failed withdrawal rolls back metadata erasure along with account deletion.
	if err := db.Exec("CREATE TABLE withdrawal_guard (user_id uuid REFERENCES users(id))").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO withdrawal_guard VALUES (?)", j.UserID).Error; err != nil {
		t.Fatal(err)
	}
	withdrawal, err := auth.NewAccountWithdrawalService(auth.NewGormWithdrawalAccountStore(db), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Config.Enabled = false
	if err = withdrawal.Withdraw(context.Background(), j.UserID); err == nil {
		t.Fatal("expected rollback")
	}
	got, err := s.store.Load().Get(context.Background(), j.ID, j.UserID)
	if err != nil || got.OriginalFilename == nil || got.Version != j.Version {
		t.Fatal("erasure escaped rollback", err)
	}
	if err = db.Exec("DROP TABLE withdrawal_guard").Error; err != nil {
		t.Fatal(err)
	}
	if err = withdrawal.Withdraw(context.Background(), j.UserID); err != nil {
		t.Fatal(err)
	}
	assertFilenameNULL(t, s, j)
	if err = s.store.Load().Save(context.Background(), &j); !errors.Is(err, ErrConflict) {
		t.Fatal("withdrawal allowed stale write", err)
	}
	got, err = s.store.Load().Get(context.Background(), other.ID, other.UserID)
	if err != nil || got.OriginalFilename == nil || *got.OriginalFilename != "other.mp4" {
		t.Fatal("other user's filename changed", err)
	}
}

func TestOriginalFilenameMigration(t *testing.T) {
	s := testService(t)
	db := s.store.Load().DB
	up, err := os.ReadFile("../database/migration/sql/000019_add_vod_original_filename.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if string(up) != filenameSQL {
		t.Fatal("auto/versioned filename drift")
	}
	down, err := os.ReadFile("../database/migration/sql/000019_add_vod_original_filename.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	j := createJob(t, s, "queued")
	j.OriginalFilename = sanitizeOriginalFilename("private.mp4")
	saveJob(t, s, &j)
	for i := 0; i < 2; i++ {
		if err = db.Exec(string(down)).Error; err != nil {
			t.Fatal(err)
		}
	}
	if db.Migrator().HasColumn(&Job{}, "original_filename") {
		t.Fatal("column survived rollback")
	}
	var count int64
	if err = db.Table("vod_jobs").Where("id = ?", j.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("rollback deleted job", err)
	}
	for i := 0; i < 2; i++ {
		if err = db.Exec(string(up)).Error; err != nil {
			t.Fatal(err)
		}
	}
	assertFilenameNULL(t, s, j)
	if err = db.Exec(string(down)).Error; err != nil {
		t.Fatal(err)
	}
	if err = AutoMigrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	assertFilenameNULL(t, s, j)
}
