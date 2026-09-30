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

// UserStatusChecker는 access token subject의 현재 상태를 조회한다. 인터페이스로 두어
// 미들웨어를 따로 테스트할 수 있고, 비활성 사용자는 보호된 핸들러가 돌기 전에
// 거절된다.
type UserStatusChecker interface {
	UserStatus(context.Context, uuid.UUID) (UserStatus, error)
}

// ErrAuthenticationRequired는 토큰으로 활성 InnoLive 사용자를 확인할 수 없을 때
// 돌려준다. 호출자는 잘못된·만료된·비활성 자격 정보를 일부러 같은 응답으로 옮긴다.
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

// RequireUser는 Bearer access token을 검증하고, subject에 아직 활성 사용자 행이
// 있는지 확인한 뒤 그 UUID를 요청 컨텍스트에 넣는다.
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

// AuthenticateUser는 access token을 검증하고 사용자가 아직 활성인지 확인한다. HTTP
// 미들웨어와 WebSocket signaling이 함께 쓴다 — signaling은 자격 정보가 HTTP 헤더가
// 아니라 signaling 메시지 안에 온다.
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

// UserIDFromContext는 RequireUser가 넣은 인증 사용자를 돌려준다.
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
