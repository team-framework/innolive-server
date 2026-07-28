package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

type testUserStatusChecker struct {
	status UserStatus
	err    error
}

func (s testUserStatusChecker) UserStatus(context.Context, uuid.UUID) (UserStatus, error) {
	return s.status, s.err
}

func TestRequireUser(t *testing.T) {
	service := testTokenService(newMemoryRefreshStore())
	userID := uuid.New()
	access, err := service.issueAccessToken(userID, uuid.New(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := UserIDFromContext(r.Context())
		if !ok || got != userID {
			t.Fatalf("context user = %s, %t; want %s, true", got, ok, userID)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	for _, tc := range []struct {
		name    string
		token   string
		checker testUserStatusChecker
		want    int
	}{
		{name: "missing token", checker: testUserStatusChecker{status: UserStatusActive}, want: http.StatusUnauthorized},
		{name: "invalid token", token: "invalid", checker: testUserStatusChecker{status: UserStatusActive}, want: http.StatusUnauthorized},
		{name: "disabled user", token: access, checker: testUserStatusChecker{status: UserStatusDisabled}, want: http.StatusUnauthorized},
		{name: "missing user", token: access, checker: testUserStatusChecker{err: ErrUserInactive}, want: http.StatusUnauthorized},
		{name: "database error", token: access, checker: testUserStatusChecker{err: errors.New("database unavailable")}, want: http.StatusInternalServerError},
		{name: "active user", token: access, checker: testUserStatusChecker{status: UserStatusActive}, want: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := RequireUser(service, tc.checker)(protected)
			request := httptest.NewRequest(http.MethodGet, "/protected", nil)
			if tc.token != "" {
				request.Header.Set("Authorization", "Bearer "+tc.token)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tc.want, response.Body.String())
			}
		})
	}
}

func TestRequireUserAllowsCORSPreflight(t *testing.T) {
	handler := RequireUser(nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodOptions, "/protected", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}
