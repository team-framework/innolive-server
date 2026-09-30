package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"inno-live-server/internal/plan"
	"inno-live-server/internal/session"

	"github.com/google/uuid"
)

func newAdminTestServer(t *testing.T) (baseURL string, manager *session.Manager, adminHeader, userHeader http.Header, userID uuid.UUID) {
	t.Helper()
	requireUser, authenticateUser, adminHeader, _, adminID := testRequireUser(t)
	_, _, userHeader, _, userID = testRequireUser(t)
	application, manager := newTestApplicationWithUserMiddleware(t, requireUser, authenticateUser)
	t.Cleanup(manager.CloseAll)
	application.cfg.AdminUserIDs = []string{adminID.String()}
	application.SetPlanStore(&memoryPlanStore{
		plans:  map[uuid.UUID]plan.Plan{adminID: plan.Spark, userID: plan.Glow},
		emails: map[uuid.UUID]string{adminID: "admin@example.com", userID: "user@example.com"},
	})
	httpServer := httptest.NewServer(application.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer.URL, manager, adminHeader, userHeader, userID
}

func createUserSession(t *testing.T, baseURL string, header http.Header) string {
	t.Helper()
	response := mustRequest(t, http.MethodPost, baseURL+"/sessions", nil, header)
	defer response.Body.Close()
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil || created.SessionID == "" {
		t.Fatalf("create session = %d, %v", response.StatusCode, err)
	}
	return created.SessionID
}

func TestAdminListsAndClosesAnySession(t *testing.T) {
	baseURL, manager, adminHeader, userHeader, userID := newAdminTestServer(t)
	sessionID := createUserSession(t, baseURL, userHeader)

	response := mustRequest(t, http.MethodGet, baseURL+"/admin/sessions", nil, adminHeader)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/sessions = %d %s", response.StatusCode, body)
	}
	if strings.Contains(string(body), "owner_token") {
		t.Fatalf("admin session list leaks owner token: %s", body)
	}
	var listed struct {
		Sessions []adminSession `json:"sessions"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 1 {
		t.Fatalf("sessions = %+v, want the user's one", listed.Sessions)
	}
	got := listed.Sessions[0]
	if got.SessionID != sessionID || got.UserID == nil || *got.UserID != userID || got.Email != "user@example.com" || got.Guest {
		t.Fatalf("listed session = %+v", got)
	}

	response = mustRequest(t, http.MethodDelete, baseURL+"/admin/sessions/"+sessionID, nil, adminHeader)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /admin/sessions = %d, want 204", response.StatusCode)
	}
	if _, err := manager.Get(sessionID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("session still alive after admin close: %v", err)
	}
	response = mustRequest(t, http.MethodDelete, baseURL+"/admin/sessions/"+sessionID, nil, adminHeader)
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("second DELETE = %d, want 404", response.StatusCode)
	}
}

func TestNonAdminCannotUseAdminRoutes(t *testing.T) {
	baseURL, manager, _, userHeader, _ := newAdminTestServer(t)
	sessionID := createUserSession(t, baseURL, userHeader)

	routes := []struct{ method, path string }{
		{http.MethodGet, "/admin/me"},
		{http.MethodGet, "/admin/sessions"},
		{http.MethodDelete, "/admin/sessions/" + sessionID},
		{http.MethodGet, "/admin/users?email=user"},
	}
	for _, route := range routes {
		response := mustRequest(t, route.method, baseURL+route.path, nil, userHeader)
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("non-admin %s %s = %d, want 403", route.method, route.path, response.StatusCode)
		}
		response = mustRequest(t, route.method, baseURL+route.path, nil, nil)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s = %d, want 401", route.method, route.path, response.StatusCode)
		}
	}
	if _, err := manager.Get(sessionID); err != nil {
		t.Fatalf("rejected admin request closed the session: %v", err)
	}
}

func TestAdminMeAndUserSearch(t *testing.T) {
	baseURL, _, adminHeader, _, userID := newAdminTestServer(t)

	response := mustRequest(t, http.MethodGet, baseURL+"/admin/me", nil, adminHeader)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/me = %d, want 200", response.StatusCode)
	}

	response = mustRequest(t, http.MethodGet, baseURL+"/admin/users?email=user@", nil, adminHeader)
	var found struct {
		Users []struct {
			UserID uuid.UUID `json:"user_id"`
			Email  string    `json:"email"`
			Plan   plan.Plan `json:"plan"`
		} `json:"users"`
	}
	if err := json.NewDecoder(response.Body).Decode(&found); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(found.Users) != 1 || found.Users[0].UserID != userID || found.Users[0].Plan != plan.Glow {
		t.Fatalf("GET /admin/users = %d %+v", response.StatusCode, found.Users)
	}
}
