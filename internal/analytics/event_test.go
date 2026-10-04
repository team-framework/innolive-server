package analytics

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

const payload = `{"version":1,"eventId":"12345678-1234-4234-8234-123456789abc","visitId":"12345678-1234-4234-8234-123456789abd","sequence":1,"event":"future_event","properties":{"new_dimension":"a","amount":3.5,"enabled":true}}`

func TestGenericEnvelope(t *testing.T) {
	if _, err := Decode(strings.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{payload + "{}", strings.Replace(payload, `"sequence":1`, `"sequence":0`, 1), strings.Replace(payload, `"amount":3.5`, `"amount":{}`, 1), strings.Replace(payload, `"new_dimension":"a"`, `"new_dimension":"\u0000"`, 1), strings.Replace(payload, `"version":1`, `"version":2`, 1)} {
		if _, err := Decode(strings.NewReader(body)); err == nil {
			t.Fatal("accepted invalid envelope")
		}
	}
}

type fakeStore struct{ calls int }

func (s *fakeStore) Save(_ context.Context, _ Event) error { s.calls++; return nil }
func TestCollectorAuthenticationAndSize(t *testing.T) {
	store := &fakeStore{}
	h := NewHandler(store, "private-key")
	for _, c := range []struct {
		body, key string
		status    int
	}{{payload, "", 401}, {payload, "Bearer private-key", 204}, {"{}", "Bearer private-key", 400}, {strings.Repeat("x", 4097), "Bearer private-key", 413}} {
		r := httptest.NewRequest("POST", "/analytics/events", strings.NewReader(c.body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", c.key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.status {
			t.Fatalf("status %d want %d", w.Code, c.status)
		}
	}
	if store.calls != 1 {
		t.Fatal("invalid request reached store")
	}
}
