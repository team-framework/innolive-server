package auth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const maxGoogleLoginRequestBody = 16 << 10

// handleGoogleLogin은 v1 구글 로그인이다 — 처음 보는 신원이면 계정을 만든다.
func (h *tokenHTTPHandler) handleGoogleLogin(w http.ResponseWriter, r *http.Request) {
	h.handleGoogleLoginMode(w, r, false)
}

// handleGoogleLoginV2는 연결된 신원만 로그인시키는 v2 구글 로그인이다(#380).
func (h *tokenHTTPHandler) handleGoogleLoginV2(w http.ResponseWriter, r *http.Request) {
	h.handleGoogleLoginMode(w, r, true)
}

func (h *tokenHTTPHandler) handleGoogleLoginMode(w http.ResponseWriter, r *http.Request, v2 bool) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		h.writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}

	rawIDToken, err := decodeGoogleLoginRequest(w, r)
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid Google login request.")
		return
	}
	login := h.google.Login
	if v2 {
		login = h.google.LoginV2
	}
	pair, err := login(r.Context(), rawIDToken, requestClientInfo(r))
	if err != nil {
		if h.writeAccountLinkLoginError(w, r, err, "Google") {
			return
		}
		switch {
		case errors.Is(err, ErrInvalidGoogleIDToken), errors.Is(err, ErrUserInactive):
			h.writeError(w, r, http.StatusUnauthorized, "invalid_google_token", "Google login could not be verified.")
		default:
			h.logger.Error("Google login failed", "request_id", tokenRequestID(r), "error", err)
			h.writeError(w, r, http.StatusInternalServerError, "internal_error", "An unexpected server error occurred.")
		}
		return
	}
	h.writeJSON(w, http.StatusOK, pair)
}

func decodeGoogleLoginRequest(w http.ResponseWriter, r *http.Request) (string, error) {
	request := struct {
		IDToken string `json:"id_token"`
	}{}
	r.Body = http.MaxBytesReader(w, r.Body, maxGoogleLoginRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return "", err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", errors.New("request body must contain one JSON object")
	}
	request.IDToken = strings.TrimSpace(request.IDToken)
	if request.IDToken == "" {
		return "", errors.New("id_token is required")
	}
	return request.IDToken, nil
}
