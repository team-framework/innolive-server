package experiencequality

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const validBody = `{"version":1,"attemptId":"12345678-1234-4234-8234-123456789abc","event":"started","role":"guest","locale":"ko","retry":false,"stage":"session","elapsedMs":0,"browser":"safari","release":"abc123"}`
const testKey = "test-service-key-at-least-32-characters"

type memoryStore struct {
	events   []Event
	err      error
	deadline bool
}

func (s *memoryStore) Save(ctx context.Context, e Event) error {
	s.events = append(s.events, e)
	_, s.deadline = ctx.Deadline()
	return s.err
}
func post(h http.Handler, body, key, contentType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/experience-quality", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestCollectorAcknowledgesOnlyPersistence(t *testing.T) {
	store := &memoryStore{}
	h := NewHandler(store, testKey)
	w := post(h, validBody, testKey, "application/json")
	if w.Code != 204 || w.Header().Get("Cache-Control") != "no-store" || len(store.events) != 1 || !store.deadline {
		t.Fatalf("successful collection: status=%d events=%d", w.Code, len(store.events))
	}
	store.err = errors.New("database private connection detail")
	w = post(h, validBody, testKey, "application/json")
	if w.Code != 503 || w.Body.Len() != 0 {
		t.Fatalf("failed persistence: %d %s", w.Code, w.Body.String())
	}
}

func TestCollectorRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name, body, key, contentType string
		status                       int
	}{
		{"auth", validBody, "wrong", "application/json", 401},
		{"type", validBody, testKey, "text/plain", 415},
		{"size", strings.Repeat("x", 1025), testKey, "application/json", 413},
		{"malformed", "{", testKey, "application/json", 400},
		{"extra", strings.TrimSuffix(validBody, "}") + `,"user_id":"private"}`, testKey, "application/json", 400},
		{"two documents", validBody + validBody, testKey, "application/json", 400},
		{"missing timing", strings.Replace(validBody, `,"elapsedMs":0`, "", 1), testKey, "application/json", 400},
		{"missing retry", strings.Replace(validBody, `,"retry":false`, "", 1), testKey, "application/json", 400},
		{"code required", strings.Replace(validBody, `"started"`, `"failed"`, 1), testKey, "application/json", 400},
		{"raw browser", strings.Replace(validBody, `"safari"`, `"Mozilla/5.0"`, 1), testKey, "application/json", 400},
		{"code unexpected", strings.TrimSuffix(validBody, "}") + `,"code":"timeout"}`, testKey, "application/json", 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &memoryStore{}
			w := post(NewHandler(store, testKey), c.body, c.key, c.contentType)
			if w.Code != c.status || len(store.events) != 0 {
				t.Fatalf("status %d; stored %d", w.Code, len(store.events))
			}
		})
	}
	// Unknown-length bodies must still obey the byte cap.
	r := httptest.NewRequest("POST", "/experience-quality", strings.NewReader(strings.Repeat("x", 1025)))
	r.ContentLength = -1
	r.Header.Set("Authorization", "Bearer "+testKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	NewHandler(&memoryStore{}, testKey).ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatalf("chunked body: %d", w.Code)
	}
}

func TestCollectorRateWindow(t *testing.T) {
	h := NewHandler(&memoryStore{}, testKey)
	now := time.Now()
	for i := 0; i < IngestPerMinute; i++ {
		if !h.allow(now) {
			t.Fatal("rejected before limit")
		}
	}
	w := post(h, validBody, testKey, "application/json")
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" {
		t.Fatalf("rate limit: %d", w.Code)
	}
	if !h.allow(now.Add(time.Minute)) {
		t.Fatal("new window rejected")
	}
}

func TestOldPayloadWithoutBrowserOrRelease(t *testing.T) {
	body := strings.Replace(validBody, `,"browser":"safari","release":"abc123"`, "", 1)
	e, err := Decode(strings.NewReader(body))
	if err != nil || e.Browser != "unknown" || e.Release != "unknown" {
		t.Fatalf("legacy: %+v %v", e, err)
	}
}

func TestMigrationSchemaParity(t *testing.T) {
	versioned, err := os.ReadFile("../database/migration/sql/000013_create_experience_quality_events.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if string(versioned) != schemaSQL {
		t.Fatal("auto and versioned collector schemas diverged")
	}
}

func TestContractFixture(t *testing.T) {
	body, err := os.ReadFile("testdata/event-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	e, err := Decode(strings.NewReader(string(body)))
	if err != nil || e.Event != "failed" || e.Browser != "safari" {
		t.Fatalf("fixture: %v", err)
	}
}
