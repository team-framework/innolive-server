package auth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const (
	maxEmailAuthRequestBody = 16 << 10
	signupTokenCookieName   = "signup_token"
)

// handleEmailSignup follows Clash's /auth/sign-up contract. The signup token
// is deliberately kept out of the JSON response and sent as an HttpOnly cookie.
func (h *tokenHTTPHandler) handleEmailSignup(w http.ResponseWriter, r *http.Request) {
	h.handleEmailSignupMode(w, r, false)
}

func (h *tokenHTTPHandler) handleNativeEmailSignup(w http.ResponseWriter, r *http.Request) {
	h.handleEmailSignupMode(w, r, true)
}

func (h *tokenHTTPHandler) handleEmailSignupMode(w http.ResponseWriter, r *http.Request, native bool) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		h.writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}

	request, err := decodeEmailSignupRequest(w, r)
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid email signup request.")
		return
	}
	token, err := h.email.StartSignup(r.Context(), request.Email, request.Password)
	if err != nil {
		switch {
		case errors.Is(err, ErrEmailSignupInvalid):
			h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid email signup request.")
		case errors.Is(err, ErrEmailAlreadyRegistered):
			h.writeError(w, r, http.StatusConflict, "email_already_registered", "Email is already registered.")
		case errors.Is(err, ErrEmailDeliveryUnavailable):
			h.writeError(w, r, http.StatusServiceUnavailable, "email_delivery_unavailable", "Email verification is temporarily unavailable.")
		default:
			h.logger.Error("email signup request failed", "request_id", tokenRequestID(r), "error", err)
			h.writeError(w, r, http.StatusBadGateway, "email_delivery_failed", "Email verification could not be sent.")
		}
		return
	}

	if native {
		h.writeJSON(w, http.StatusOK, map[string]string{"status": "verification_email_sent", "signup_token": token})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     signupTokenCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(h.email.config.CodeTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteNoneMode,
	})
	h.writeJSON(w, http.StatusOK, map[string]string{"status": "verification_email_sent"})
}

// handleEmailSignupVerification follows Clash's /auth/verify-email contract:
// it reads signup_token from the HttpOnly cookie and accepts only the code.
func (h *tokenHTTPHandler) handleEmailSignupVerification(w http.ResponseWriter, r *http.Request) {
	h.handleEmailSignupVerificationMode(w, r, false)
}

func (h *tokenHTTPHandler) handleNativeEmailSignupVerification(w http.ResponseWriter, r *http.Request) {
	h.handleEmailSignupVerificationMode(w, r, true)
}

func (h *tokenHTTPHandler) handleEmailSignupVerificationMode(w http.ResponseWriter, r *http.Request, native bool) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		h.writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}

	request, err := decodeEmailVerificationRequest(w, r)
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid email verification request.")
		return
	}
	var signupToken string
	if native {
		signupToken = strings.TrimSpace(request.SignupToken)
	} else {
		cookie, err := r.Cookie(signupTokenCookieName)
		if err == nil {
			signupToken = strings.TrimSpace(cookie.Value)
		}
	}
	if signupToken == "" {
		h.writeError(w, r, http.StatusBadRequest, "invalid_signup_token", "Signup verification session is invalid or expired.")
		return
	}
	if err := h.email.CompleteSignup(r.Context(), signupToken, request.VerificationCode); err != nil {
		switch {
		case errors.Is(err, ErrEmailVerificationInvalid):
			h.writeError(w, r, http.StatusBadRequest, "invalid_verification_code", "Verification code is invalid or expired.")
		case errors.Is(err, ErrEmailAlreadyRegistered):
			h.writeError(w, r, http.StatusConflict, "email_already_registered", "Email is already registered.")
		case errors.Is(err, ErrEmailDeliveryUnavailable):
			h.writeError(w, r, http.StatusServiceUnavailable, "email_auth_unavailable", "Email authentication is temporarily unavailable.")
		default:
			h.logger.Error("email signup verification failed", "request_id", tokenRequestID(r), "error", err)
			h.writeError(w, r, http.StatusInternalServerError, "internal_error", "An unexpected server error occurred.")
		}
		return
	}

	if !native {
		http.SetCookie(w, &http.Cookie{Name: signupTokenCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteNoneMode})
	}
	h.writeJSON(w, http.StatusOK, map[string]string{"status": "email_verified"})
}

func (h *tokenHTTPHandler) handleEmailLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		h.writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}

	request, err := decodeEmailLoginRequest(w, r)
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid email login request.")
		return
	}
	pair, err := h.email.Login(r.Context(), request.Email, request.Password, requestClientInfo(r))
	if err != nil {
		switch {
		case errors.Is(err, ErrEmailCredentialsInvalid), errors.Is(err, ErrUserInactive):
			h.writeError(w, r, http.StatusUnauthorized, "invalid_email_credentials", "Email or password is invalid.")
		case errors.Is(err, ErrEmailDeliveryUnavailable):
			h.writeError(w, r, http.StatusServiceUnavailable, "email_auth_unavailable", "Email authentication is temporarily unavailable.")
		default:
			h.logger.Error("email login failed", "request_id", tokenRequestID(r), "error", err)
			h.writeError(w, r, http.StatusInternalServerError, "internal_error", "An unexpected server error occurred.")
		}
		return
	}
	h.writeJSON(w, http.StatusOK, pair)
}

type emailSignupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type emailVerificationRequest struct {
	SignupToken      string `json:"signup_token"`
	VerificationCode string `json:"verification_code"`
}

type emailLoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func decodeEmailSignupRequest(w http.ResponseWriter, r *http.Request) (emailSignupRequest, error) {
	var request emailSignupRequest
	if err := decodeSingleJSON(w, r, &request); err != nil {
		return emailSignupRequest{}, err
	}
	request.Email = strings.TrimSpace(request.Email)
	if request.Email == "" || request.Password == "" {
		return emailSignupRequest{}, errors.New("email and password are required")
	}
	return request, nil
}

func decodeEmailVerificationRequest(w http.ResponseWriter, r *http.Request) (emailVerificationRequest, error) {
	var request emailVerificationRequest
	if err := decodeSingleJSON(w, r, &request); err != nil {
		return emailVerificationRequest{}, err
	}
	request.VerificationCode = strings.TrimSpace(request.VerificationCode)
	if request.VerificationCode == "" {
		return emailVerificationRequest{}, errors.New("verification_code is required")
	}
	return request, nil
}

func decodeEmailLoginRequest(w http.ResponseWriter, r *http.Request) (emailLoginRequest, error) {
	var request emailLoginRequest
	if err := decodeSingleJSON(w, r, &request); err != nil {
		return emailLoginRequest{}, err
	}
	request.Email = strings.TrimSpace(request.Email)
	if request.Email == "" || request.Password == "" {
		return emailLoginRequest{}, errors.New("email and password are required")
	}
	return request, nil
}

func decodeSingleJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxEmailAuthRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}
