package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const maxTokenRequestBody = 8 << 10

type tokenHTTPHandler struct {
	service           *TokenService
	google            *GoogleLoginService
	apple             *AppleLoginService
	email             *EmailAuthService
	withdrawal        *AccountWithdrawalService
	youtube           *YouTubeConnectService
	streamingAccounts *StreamingAccountService
	logger            *slog.Logger
	config            TokenHTTPConfig
}

func MountTokenHTTP(next http.Handler, service *TokenService, logger *slog.Logger, config TokenHTTPConfig) http.Handler {
	return MountAuthHTTP(next, service, nil, logger, config)
}

func MountAuthHTTP(next http.Handler, service *TokenService, google *GoogleLoginService, logger *slog.Logger, config TokenHTTPConfig, appleServices ...*AppleLoginService) http.Handler {
	var apple *AppleLoginService
	if len(appleServices) > 0 {
		apple = appleServices[0]
	}
	return mountAuthHTTP(next, service, google, apple, nil, nil, nil, nil, logger, config)
}

// MountAuthHTTPWithWithdrawal adds account deletion to the authentication
// routes. It is kept separate to preserve the existing constructor used by
// smaller deployments and focused handler tests.
func MountAuthHTTPWithWithdrawal(next http.Handler, service *TokenService, google *GoogleLoginService, apple *AppleLoginService, withdrawal *AccountWithdrawalService, logger *slog.Logger, config TokenHTTPConfig) http.Handler {
	return mountAuthHTTP(next, service, google, apple, nil, withdrawal, nil, nil, logger, config)
}

// MountAuthHTTPWithServices mounts all configured authentication services.
// youtubeServices는 송출 연동(YouTube OAuth)을 함께 마운트할 때만 전달한다 —
// 기존 호출부(테스트 포함)를 깨지 않도록 가변 인자로 확장했다.
func MountAuthHTTPWithServices(next http.Handler, service *TokenService, google *GoogleLoginService, apple *AppleLoginService, email *EmailAuthService, withdrawal *AccountWithdrawalService, logger *slog.Logger, config TokenHTTPConfig, youtubeServices ...*YouTubeConnectService) http.Handler {
	var youtube *YouTubeConnectService
	if len(youtubeServices) > 0 {
		youtube = youtubeServices[0]
	}
	return mountAuthHTTP(next, service, google, apple, email, withdrawal, youtube, nil, logger, config)
}

// MountAuthHTTPWithStreaming은 송출 계정 조회·해제(#88)까지 포함해 전체
// 인증 라우트를 마운트한다 — 프로덕션 조립(main)이 쓰는 완전형이다.
func MountAuthHTTPWithStreaming(next http.Handler, service *TokenService, google *GoogleLoginService, apple *AppleLoginService, email *EmailAuthService, withdrawal *AccountWithdrawalService, youtube *YouTubeConnectService, streamingAccounts *StreamingAccountService, logger *slog.Logger, config TokenHTTPConfig) http.Handler {
	return mountAuthHTTP(next, service, google, apple, email, withdrawal, youtube, streamingAccounts, logger, config)
}

func mountAuthHTTP(next http.Handler, service *TokenService, google *GoogleLoginService, apple *AppleLoginService, email *EmailAuthService, withdrawal *AccountWithdrawalService, youtube *YouTubeConnectService, streamingAccounts *StreamingAccountService, logger *slog.Logger, config TokenHTTPConfig) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &tokenHTTPHandler{service: service, google: google, apple: apple, email: email, withdrawal: withdrawal, youtube: youtube, streamingAccounts: streamingAccounts, logger: logger, config: config}
	mux := http.NewServeMux()
	mux.Handle("/auth/refresh", h.middleware(http.HandlerFunc(h.handleRefresh)))
	mux.Handle("/auth/logout", h.middleware(http.HandlerFunc(h.handleLogout)))
	if google != nil {
		mux.Handle("/auth/google", h.middleware(http.HandlerFunc(h.handleGoogleLogin)))
	}
	if h.apple != nil {
		mux.Handle("/auth/apple", h.middleware(http.HandlerFunc(h.handleAppleLogin)))
	}
	if h.email != nil {
		mux.Handle("/auth/sign-up", h.middleware(http.HandlerFunc(h.handleEmailSignup)))
		mux.Handle("/auth/verify-email", h.middleware(http.HandlerFunc(h.handleEmailSignupVerification)))
		mux.Handle("/auth/native/sign-up", h.middleware(http.HandlerFunc(h.handleNativeEmailSignup)))
		mux.Handle("/auth/native/verify-email", h.middleware(http.HandlerFunc(h.handleNativeEmailSignupVerification)))
		mux.Handle("/auth/sign-in", h.middleware(http.HandlerFunc(h.handleEmailLogin)))
	}
	if h.withdrawal != nil {
		mux.Handle("DELETE /auth/me", h.middleware(http.HandlerFunc(h.handleWithdrawal)))
	}
	if h.youtube != nil {
		mux.Handle("POST /auth/youtube/connect", h.middleware(http.HandlerFunc(h.handleYouTubeConnect)))
		mux.Handle("GET /auth/youtube/config", h.middleware(http.HandlerFunc(h.handleYouTubeConfig)))
	}
	if h.streamingAccounts != nil {
		mux.Handle("GET /auth/streaming/accounts", h.middleware(http.HandlerFunc(h.handleListStreamingAccounts)))
	}
	mux.Handle("/", next)
	return mux
}

func (h *tokenHTTPHandler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		h.writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}

	raw, err := decodeRefreshRequest(w, r)
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid refresh token request.")
		return
	}
	pair, err := h.service.Rotate(r.Context(), raw, requestClientInfo(r))
	if err != nil {
		switch {
		case errors.Is(err, ErrRefreshTokenReused):
			h.logger.Warn("refresh token reuse detected", "request_id", tokenRequestID(r))
			h.writeError(w, r, http.StatusUnauthorized, "invalid_refresh_token", "Refresh token is invalid.")
		case errors.Is(err, ErrInvalidRefreshToken),
			errors.Is(err, ErrRefreshTokenExpired),
			errors.Is(err, ErrRefreshTokenRevoked),
			errors.Is(err, ErrUserInactive):
			h.writeError(w, r, http.StatusUnauthorized, "invalid_refresh_token", "Refresh token is invalid.")
		default:
			h.logger.Error("refresh token rotation failed", "request_id", tokenRequestID(r), "error", err)
			h.writeError(w, r, http.StatusInternalServerError, "internal_error", "An unexpected server error occurred.")
		}
		return
	}
	h.writeJSON(w, http.StatusOK, pair)
}

func (h *tokenHTTPHandler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		h.writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}

	raw, err := decodeRefreshRequest(w, r)
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid logout request.")
		return
	}
	if err := h.service.Logout(r.Context(), raw); err != nil {
		if !errors.Is(err, ErrInvalidRefreshToken) &&
			!errors.Is(err, ErrRefreshTokenExpired) &&
			!errors.Is(err, ErrRefreshTokenRevoked) &&
			!errors.Is(err, ErrRefreshTokenReused) &&
			!errors.Is(err, ErrUserInactive) {
			h.logger.Error("refresh token logout failed", "request_id", tokenRequestID(r), "error", err)
			h.writeError(w, r, http.StatusInternalServerError, "internal_error", "An unexpected server error occurred.")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeRefreshRequest(w http.ResponseWriter, r *http.Request) (string, error) {
	request := struct {
		RefreshToken string `json:"refresh_token"`
	}{}
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return "", err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", errors.New("request body must contain one JSON object")
	}
	request.RefreshToken = strings.TrimSpace(request.RefreshToken)
	if request.RefreshToken == "" {
		return "", errors.New("refresh_token is required")
	}
	return request.RefreshToken, nil
}

func requestClientInfo(r *http.Request) ClientInfo {
	return ClientInfo{
		UserAgent: truncateTokenMetadata(strings.TrimSpace(r.UserAgent()), 512),
		IPAddress: tokenRemoteIP(r.RemoteAddr),
	}
}

func tokenRemoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err == nil {
		return host
	}
	return strings.TrimSpace(remoteAddr)
}

func truncateTokenMetadata(value string, limit int) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func (h *tokenHTTPHandler) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if id == "" {
			id = uuid.NewString()
			r.Header.Set("X-Request-ID", id)
		}
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != "" {
			allowedOrigin, ok := h.config.AllowedOrigin(origin)
			if !ok {
				h.writeError(w, r, http.StatusForbidden, "origin_not_allowed", "Origin is not allowed.")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")
			if allowedOrigin != "*" {
				w.Header().Add("Vary", "Origin")
			}
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		next.ServeHTTP(w, r)
	})
}

func tokenRequestID(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Request-ID"))
}

func (h *tokenHTTPHandler) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	h.writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
		"request_id": tokenRequestID(r),
	})
}

func (h *tokenHTTPHandler) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		_, _ = fmt.Fprintln(w, `{}`)
	}
}
