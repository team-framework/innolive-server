package server

import (
	"context"
	"inno-live-server/internal/experiencequality"
	"net/http/httptest"
	"strings"
	"testing"
)

type qualityRouteStore struct{ count int }

func (s *qualityRouteStore) Save(context.Context, experiencequality.Event) error {
	s.count++
	return nil
}

func TestExperienceQualityRoute(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			app, manager := newTestApplication(t)
			defer manager.CloseAll()
			key := "test-service-key-at-least-32-characters"
			if enabled {
				app.cfg.ExperienceQualityIngestKey = key
			}
			store := &qualityRouteStore{}
			app.SetExperienceQuality(store)
			request := httptest.NewRequest("POST", "/experience-quality", strings.NewReader(`{"version":1,"attemptId":"12345678-1234-4234-8234-123456789abc","event":"started","role":"guest","locale":"ko","retry":false,"stage":"session","elapsedMs":0}`))
			request.Header.Set("Authorization", "Bearer "+key)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			app.Handler().ServeHTTP(response, request)
			if enabled {
				if response.Code != 204 || store.count != 1 {
					t.Fatalf("enabled collector: %d %d", response.Code, store.count)
				}
			} else if response.Code != 404 || store.count != 0 {
				t.Fatalf("disabled collector: %d %d", response.Code, store.count)
			}
		})
	}
}
