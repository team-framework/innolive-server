package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// writeAccountLinkLoginError는 구글·애플 로그인의 #380 거절을 응답한다. 처리했으면 true다.
//   - 연결 안 된 신원: 403 account_not_linked — InnoLive 계정을 먼저 만들고 연결해야 한다
//   - 이메일 계정 없는 기존 가입자: 403 password_setup_required + setup_token
func (h *tokenHTTPHandler) writeAccountLinkLoginError(w http.ResponseWriter, r *http.Request, err error, provider string) bool {
	if errors.Is(err, ErrAccountNotLinked) {
		h.writeError(w, r, http.StatusForbidden, "account_not_linked", "Create an InnoLive account first, then link "+provider+" in settings.")
		return true
	}
	var setup *PasswordSetupRequiredError
	if errors.As(err, &setup) {
		h.writeJSON(w, http.StatusForbidden, map[string]any{
			"error": map[string]string{
				"code":    "password_setup_required",
				"message": "Set an email and password to keep using this account.",
			},
			"setup_token": setup.SetupToken,
			"request_id":  tokenRequestID(r),
		})
		return true
	}
	return false
}

// handleNativeAccountSetup은 #380 이전 OAuth 전용 가입자가 이메일 계정을 만드는
// 첫 단계다. 인증 코드를 보내고 signup_token을 돌려준다. 다음 단계는 기존
// /auth/native/verify-email이다.
func (h *tokenHTTPHandler) handleNativeAccountSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		h.writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}
	request := struct {
		SetupToken string `json:"setup_token"`
		Email      string `json:"email"`
		Password   string `json:"password"`
	}{}
	if err := decodeSingleJSON(w, r, &request); err != nil || strings.TrimSpace(request.SetupToken) == "" || strings.TrimSpace(request.Email) == "" || request.Password == "" {
		h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid account setup request.")
		return
	}
	token, err := h.email.StartAccountSetup(r.Context(), request.SetupToken, request.Email, request.Password, requestClientInfo(r).IPAddress)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidSetupToken):
			h.writeError(w, r, http.StatusUnauthorized, "invalid_setup_token", "Account setup has expired. Sign in again.")
		case errors.Is(err, ErrEmailSignupInvalid):
			h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid account setup request.")
		case errors.Is(err, ErrEmailSignupThrottled):
			h.writeError(w, r, http.StatusTooManyRequests, "too_many_requests", "Too many signup requests. Please try again later.")
		case errors.Is(err, ErrEmailAlreadyRegistered):
			h.writeError(w, r, http.StatusConflict, "email_already_registered", "This email already has an InnoLive account. Sign in with it and link this login in settings.")
		case errors.Is(err, ErrEmailDeliveryUnavailable):
			h.writeError(w, r, http.StatusServiceUnavailable, "email_delivery_unavailable", "Email verification is temporarily unavailable.")
		default:
			h.logger.Error("account setup request failed", "request_id", tokenRequestID(r), "error", err)
			h.writeError(w, r, http.StatusBadGateway, "email_delivery_failed", "Email verification could not be sent.")
		}
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]string{"status": "verification_email_sent", "signup_token": token})
}

// MountAccountLinkHTTP는 로그인한 사용자의 구글·애플 연결·해제와 로그인 수단 조회를
// 붙인다(#380). closeMergedUserSessions는 병합으로 사라진 사용자의 메모리 세션을 닫는다.
func MountAccountLinkHTTP(next http.Handler, service *TokenService, links *AccountLinkStore, google *GoogleLoginService, apple *AppleLoginService, logger *slog.Logger, config TokenHTTPConfig, closeMergedUserSessions func(uuid.UUID)) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &tokenHTTPHandler{service: service, google: google, apple: apple, logger: logger, config: config, logoutSessionCloser: closeMergedUserSessions}
	link := &accountLinkHandler{tokenHTTPHandler: h, links: links}
	mux := http.NewServeMux()
	if google != nil {
		mux.Handle("POST /auth/link/google", h.middleware(http.HandlerFunc(link.handleLinkGoogle)))
	}
	if apple != nil {
		mux.Handle("POST /auth/link/apple", h.middleware(http.HandlerFunc(link.handleLinkApple)))
	}
	mux.Handle("DELETE /auth/link/{provider}", h.middleware(http.HandlerFunc(link.handleUnlink)))
	mux.Handle("DELETE /auth/link/{provider}/{id}", h.middleware(http.HandlerFunc(link.handleUnlink)))
	mux.Handle("GET /auth/login-methods", h.middleware(http.HandlerFunc(link.handleLoginMethods)))
	preflight := h.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	mux.Handle("OPTIONS /auth/link/", preflight)
	mux.Handle("OPTIONS /auth/login-methods", preflight)
	mux.Handle("/", next)
	return mux
}

type accountLinkHandler struct {
	*tokenHTTPHandler
	links *AccountLinkStore
}

func (h *accountLinkHandler) handleLinkGoogle(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.authenticatedUserID(w, r)
	if !ok {
		return
	}
	rawIDToken, err := decodeGoogleLoginRequest(w, r)
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid Google link request.")
		return
	}
	result, err := h.google.Link(r.Context(), userID, rawIDToken)
	if err != nil {
		if errors.Is(err, ErrInvalidGoogleIDToken) {
			h.writeError(w, r, http.StatusUnauthorized, "invalid_google_token", "Google login could not be verified.")
			return
		}
		h.writeLinkError(w, r, err, "Google")
		return
	}
	h.finishLink(w, r, userID, result)
}

func (h *accountLinkHandler) handleLinkApple(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.authenticatedUserID(w, r)
	if !ok {
		return
	}
	request, err := decodeAppleLoginRequest(w, r)
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid Apple link request.")
		return
	}
	result, err := h.apple.Link(r.Context(), userID, request.AuthorizationCode, request.Nonce)
	if err != nil {
		if errors.Is(err, ErrInvalidAppleIDToken) {
			h.writeError(w, r, http.StatusUnauthorized, "invalid_apple_token", "Apple login could not be verified.")
			return
		}
		h.writeLinkError(w, r, err, "Apple")
		return
	}
	h.finishLink(w, r, userID, result)
}

func (h *accountLinkHandler) writeLinkError(w http.ResponseWriter, r *http.Request, err error, provider string) {
	switch {
	case errors.Is(err, ErrProviderAlreadyLinked):
		h.writeError(w, r, http.StatusConflict, "provider_already_linked", "Another "+provider+" account is already linked. Unlink it first.")
	case errors.Is(err, ErrGoogleLinkLimit):
		h.writeError(w, r, http.StatusConflict, "google_link_limit", "Up to 5 Google accounts can be linked.")
	case errors.Is(err, ErrIdentityLinkedElsewhere):
		h.writeError(w, r, http.StatusConflict, "identity_linked_elsewhere", "This "+provider+" account is linked to another InnoLive account.")
	case errors.Is(err, ErrUserInactive):
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
	default:
		h.logger.Error("account link failed", "provider", provider, "request_id", tokenRequestID(r), "error", err)
		h.writeError(w, r, http.StatusBadGateway, "link_failed", provider+" could not be linked.")
	}
}

// finishLink는 병합으로 사라진 사용자의 세션을 닫고 결과를 돌려준다.
func (h *accountLinkHandler) finishLink(w http.ResponseWriter, r *http.Request, userID uuid.UUID, result LinkResult) {
	merged := result.MergedUserID != nil
	if merged {
		h.logger.Info("account merged by link", "user_id", userID, "merged_user_id", *result.MergedUserID, "provider", result.Provider, "request_id", tokenRequestID(r))
		if h.logoutSessionCloser != nil {
			h.logoutSessionCloser(*result.MergedUserID)
		}
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"provider": result.Provider, "merged": merged})
}

func (h *accountLinkHandler) handleUnlink(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.authenticatedUserID(w, r)
	if !ok {
		return
	}
	provider := OAuthProvider(r.PathValue("provider"))
	if provider != OAuthProviderGoogle && provider != OAuthProviderApple {
		h.writeError(w, r, http.StatusBadRequest, "bad_request", "provider must be google or apple.")
		return
	}
	var linkID *uuid.UUID
	if raw := r.PathValue("id"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid link id.")
			return
		}
		linkID = &parsed
	}
	switch err := h.links.Unlink(r.Context(), userID, provider, linkID); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrIdentityNotLinked):
		h.writeError(w, r, http.StatusNotFound, "not_linked", "This login is not linked.")
	case errors.Is(err, ErrMultipleLinks):
		h.writeError(w, r, http.StatusConflict, "multiple_links", "Several accounts of this provider are linked. Unlink one by id.")
	case errors.Is(err, ErrLastLoginMethod):
		h.writeError(w, r, http.StatusConflict, "last_login_method", "Set an email and password before unlinking the last login.")
	case errors.Is(err, ErrUserInactive):
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
	default:
		h.logger.Error("account unlink failed", "provider", provider, "request_id", tokenRequestID(r), "error", err)
		h.writeError(w, r, http.StatusInternalServerError, "internal_error", "An unexpected server error occurred.")
	}
}

func (h *accountLinkHandler) handleLoginMethods(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.authenticatedUserID(w, r)
	if !ok {
		return
	}
	methods, err := h.links.Methods(r.Context(), userID)
	if err != nil {
		h.logger.Error("login methods lookup failed", "request_id", tokenRequestID(r), "error", err)
		h.writeError(w, r, http.StatusInternalServerError, "internal_error", "An unexpected server error occurred.")
		return
	}
	h.writeJSON(w, http.StatusOK, methods)
}
