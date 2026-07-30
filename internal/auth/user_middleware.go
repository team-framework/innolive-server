package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type userContextKey struct{}

// UserStatusChecker resolves the current state of an access-token subject.
// Keeping it as an interface makes the middleware independently testable and
// ensures a disabled user is rejected before any protected handler runs.
type UserStatusChecker interface {
	UserStatus(context.Context, uuid.UUID) (UserStatus, error)
}

// ErrAuthenticationRequired is returned when a token cannot establish an
// active InnoLive user. Callers deliberately map it to the same response for
// malformed, expired, and inactive credentials.
var ErrAuthenticationRequired = errors.New("authentication required")

type gormUserStatusChecker struct {
	db *gorm.DB
}

func NewGormUserStatusChecker(db *gorm.DB) UserStatusChecker {
	return &gormUserStatusChecker{db: db}
}

func (s *gormUserStatusChecker) UserStatus(ctx context.Context, userID uuid.UUID) (UserStatus, error) {
	if s == nil || s.db == nil {
		return "", errors.New("user status checker database is nil")
	}
	var row struct {
		Status UserStatus `gorm:"column:status"`
	}
	result := s.db.WithContext(ctx).Model(&User{}).Select("status").Where("id = ?", userID).Take(&row)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return "", ErrUserInactive
	}
	if result.Error != nil {
		return "", result.Error
	}
	return row.Status, nil
}

// RequireUser validates a Bearer access token, confirms the subject still has
// an active user record, and stores that UUID in the request context.
func RequireUser(service *TokenService, users UserStatusChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			raw, ok := accessBearerToken(r)
			if !ok {
				writeUserMiddlewareError(w, http.StatusUnauthorized, "Authentication is required.")
				return
			}
			userID, err := AuthenticateUser(r.Context(), service, users, raw)
			if errors.Is(err, ErrAuthenticationRequired) || errors.Is(err, ErrUserInactive) {
				writeUserMiddlewareError(w, http.StatusUnauthorized, "Authentication is required.")
				return
			}
			if err != nil {
				writeUserMiddlewareError(w, http.StatusInternalServerError, "Authentication is unavailable.")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, userID)))
		})
	}
}

// AuthenticateUser validates an access token and confirms its user is still
// active. It is shared by HTTP middleware and WebSocket signaling, whose
// credentials arrive inside a signaling message rather than an HTTP header.
func AuthenticateUser(ctx context.Context, service *TokenService, users UserStatusChecker, raw string) (uuid.UUID, error) {
	if service == nil || users == nil {
		return uuid.Nil, errors.New("authentication service is unavailable")
	}
	claims, err := service.ValidateAccessToken(raw)
	if err != nil {
		return uuid.Nil, ErrAuthenticationRequired
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil, ErrAuthenticationRequired
	}
	status, err := users.UserStatus(ctx, userID)
	if errors.Is(err, ErrUserInactive) {
		return uuid.Nil, ErrAuthenticationRequired
	}
	if err != nil {
		return uuid.Nil, err
	}
	if status != UserStatusActive {
		return uuid.Nil, ErrAuthenticationRequired
	}
	return userID, nil
}

// UserIDFromContext returns the authenticated user set by RequireUser.
func UserIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	userID, ok := ctx.Value(userContextKey{}).(uuid.UUID)
	return userID, ok
}

func accessBearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

func writeUserMiddlewareError(w http.ResponseWriter, status int, message string) {
	code := "internal_error"
	if status == http.StatusUnauthorized {
		code = "unauthorized"
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}
