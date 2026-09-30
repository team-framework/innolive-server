package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/plan"

	"github.com/google/uuid"
)

type memoryPlanStore struct {
	mu     sync.Mutex
	plans  map[uuid.UUID]plan.Plan
	emails map[uuid.UUID]string
}

func (s *memoryPlanStore) SearchUsers(_ context.Context, query string, limit int) ([]auth.AdminUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []auth.AdminUser
	for id, value := range s.plans {
		if strings.Contains(s.emails[id], query) && len(result) < limit {
			result = append(result, auth.AdminUser{ID: id, Email: s.emails[id], Plan: value})
		}
	}
	return result, nil
}

func (s *memoryPlanStore) UsersByID(_ context.Context, ids []uuid.UUID) ([]auth.AdminUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []auth.AdminUser
	for _, id := range ids {
		if value, ok := s.plans[id]; ok {
			result = append(result, auth.AdminUser{ID: id, Email: s.emails[id], Plan: value})
		}
	}
	return result, nil
}

func (s *memoryPlanStore) UserPlan(_ context.Context, userID uuid.UUID) (plan.Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.plans[userID]
	if !ok {
		return "", auth.ErrUserNotFound
	}
	return value, nil
}

func (s *memoryPlanStore) SetUserPlan(_ context.Context, userID uuid.UUID, value plan.Plan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.plans[userID]; !ok {
		return auth.ErrUserNotFound
	}
	s.plans[userID] = value
	return nil
}

func putPlan(t *testing.T, baseURL string, userID uuid.UUID, body string, headers http.Header) *http.Response {
	t.Helper()
	return mustRequest(t, http.MethodPut, baseURL+"/admin/users/"+userID.String()+"/plan", strings.NewReader(body), headers)
}

func TestAdminSetsPlanAndSessionCarriesIt(t *testing.T) {
	requireUser, authenticateUser, adminHeader, _, adminID := testRequireUser(t)
	_, _, userHeader, _, userID := testRequireUser(t)
	application, manager := newTestApplicationWithUserMiddleware(t, requireUser, authenticateUser)
	defer manager.CloseAll()
	application.cfg.AdminUserIDs = []string{adminID.String()}
	store := &memoryPlanStore{plans: map[uuid.UUID]plan.Plan{adminID: plan.Spark, userID: plan.Spark}}
	application.SetPlanStore(store)
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	response := putPlan(t, httpServer.URL, userID, `{"plan":"plasma"}`, adminHeader)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("admin set plan status = %d, want 200", response.StatusCode)
	}

	response = mustRequest(t, http.MethodGet, httpServer.URL+"/users/me/plan", nil, userHeader)
	var got planResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || got.Plan != plan.Plasma || got.MonthlyBroadcastSeconds != 240*3600 || len(got.AllowedModes) != 4 {
		t.Fatalf("GET /users/me/plan = %d %+v", response.StatusCode, got)
	}

	response = mustRequest(t, http.MethodPost, httpServer.URL+"/sessions", nil, userHeader)
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	liveSession, err := manager.Get(created.SessionID)
	if err != nil {
		t.Fatalf("created session missing: %v (status %d)", err, response.StatusCode)
	}
	if liveSession.Plan != plan.Plasma {
		t.Fatalf("session plan = %q, want plasma", liveSession.Plan)
	}
}

func TestNonAdminCannotSetPlan(t *testing.T) {
	requireUser, authenticateUser, userHeader, _, userID := testRequireUser(t)
	application, manager := newTestApplicationWithUserMiddleware(t, requireUser, authenticateUser)
	defer manager.CloseAll()
	application.cfg.AdminUserIDs = []string{uuid.NewString()}
	store := &memoryPlanStore{plans: map[uuid.UUID]plan.Plan{userID: plan.Spark}}
	application.SetPlanStore(store)
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	// 자기 자신의 플랜도 올릴 수 없다.
	response := putPlan(t, httpServer.URL, userID, `{"plan":"plasma"}`, userHeader)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin set plan status = %d, want 403", response.StatusCode)
	}
	response = putPlan(t, httpServer.URL, userID, `{"plan":"plasma"}`, nil)
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated set plan status = %d, want 401", response.StatusCode)
	}
	if value, _ := store.UserPlan(context.Background(), userID); value != plan.Spark {
		t.Fatalf("plan changed to %q by rejected request", value)
	}
}

func TestAdminPlanRequestValidation(t *testing.T) {
	requireUser, authenticateUser, adminHeader, _, adminID := testRequireUser(t)
	application, manager := newTestApplicationWithUserMiddleware(t, requireUser, authenticateUser)
	defer manager.CloseAll()
	application.cfg.AdminUserIDs = []string{adminID.String()}
	application.SetPlanStore(&memoryPlanStore{plans: map[uuid.UUID]plan.Plan{adminID: plan.Spark}})
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	cases := []struct {
		name   string
		target string
		body   string
		want   int
	}{
		{"unknown plan", adminID.String(), `{"plan":"gold"}`, http.StatusBadRequest},
		{"missing plan", adminID.String(), `{}`, http.StatusBadRequest},
		{"bad user id", "not-a-uuid", `{"plan":"beam"}`, http.StatusBadRequest},
		{"unknown user", uuid.NewString(), `{"plan":"beam"}`, http.StatusNotFound},
	}
	for _, test := range cases {
		response := mustRequest(t, http.MethodPut, httpServer.URL+"/admin/users/"+test.target+"/plan", strings.NewReader(test.body), adminHeader)
		response.Body.Close()
		if response.StatusCode != test.want {
			t.Fatalf("%s: status = %d, want %d", test.name, response.StatusCode, test.want)
		}
	}
}
