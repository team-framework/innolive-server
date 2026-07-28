package auth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const maxGoogleLoginRequestBody = 16 << 10

func (h *tokenHTTPHandler) handleGoogleLogin(w http.ResponseWriter, r *http.Request) {
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
	pair, err := h.google.Login(r.Context(), rawIDToken, requestClientInfo(r))
	if err != nil {
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
