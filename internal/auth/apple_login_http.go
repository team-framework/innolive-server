package auth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const maxAppleLoginRequestBody = 16 << 10

func (h *tokenHTTPHandler) handleAppleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		h.writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}
	request, err := decodeAppleLoginRequest(w, r)
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid Apple login request.")
		return
	}
	pair, err := h.apple.Login(r.Context(), request.AuthorizationCode, request.Nonce, request.displayName(), requestClientInfo(r))
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidAppleIDToken), errors.Is(err, ErrUserInactive):
			h.writeError(w, r, http.StatusUnauthorized, "invalid_apple_token", "Apple login could not be verified.")
		default:
			h.logger.Error("Apple login failed", "request_id", tokenRequestID(r), "error", err)
			h.writeError(w, r, http.StatusBadGateway, "apple_login_failed", "Apple login could not be completed.")
		}
		return
	}
	h.writeJSON(w, http.StatusOK, pair)
}

type appleLoginRequest struct {
	AuthorizationCode string `json:"authorization_code"`
	Nonce             string `json:"nonce"`
	GivenName         string `json:"given_name"`
	FamilyName        string `json:"family_name"`
}

func decodeAppleLoginRequest(w http.ResponseWriter, r *http.Request) (appleLoginRequest, error) {
	var request appleLoginRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxAppleLoginRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return appleLoginRequest{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return appleLoginRequest{}, errors.New("request body must contain one JSON object")
	}
	request.AuthorizationCode = strings.TrimSpace(request.AuthorizationCode)
	request.Nonce = strings.TrimSpace(request.Nonce)
	if request.AuthorizationCode == "" || len(request.AuthorizationCode) > 4096 || len(request.Nonce) > 1024 {
		return appleLoginRequest{}, errors.New("authorization_code is required")
	}
	return request, nil
}

func (r appleLoginRequest) displayName() string {
	return strings.TrimSpace(strings.Join([]string{r.GivenName, r.FamilyName}, " "))
}
