package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// handleYouTubeConnect는 로그인된 사용자의 YouTube 연결 플로우를 시작한다.
// 브라우저 리다이렉트가 아니라 XHR(POST)이므로 Bearer 인증이 가능하다 —
// 응답의 authorize_url로 클라이언트가 직접 이동하는 2단계 구조다.
func (h *tokenHTTPHandler) handleYouTubeConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	raw, ok := accessBearerToken(r)
	if !ok {
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return
	}
	claims, err := h.service.ValidateAccessToken(raw)
	if err != nil {
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return
	}
	authorizeURL, err := h.youtube.Connect(r.Context(), userID)
	if err != nil {
		if errors.Is(err, ErrUserInactive) {
			h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
			return
		}
		h.logger.Error("YouTube connect failed", "request_id", tokenRequestID(r), "error", err)
		h.writeError(w, r, http.StatusInternalServerError, "internal_error", "An unexpected server error occurred.")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]string{"authorize_url": authorizeURL})
}

// handleYouTubeCallback은 Google 동의 후 브라우저 리다이렉트로 도착한다.
// Authorization 헤더가 없으므로 state가 유일한 사용자 바인딩이다. Google은
// 문서에 없는 파라미터(iss 등, 2026-08-09 실측)를 붙여 보내므로 명시한
// 파라미터만 읽고 나머지는 무시한다.
func (h *tokenHTTPHandler) handleYouTubeCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	// 사용자가 동의 화면에서 거부하면 code 없이 error=access_denied로 온다.
	if oauthError := strings.TrimSpace(query.Get("error")); oauthError != "" {
		h.writeError(w, r, http.StatusBadRequest, "youtube_authorization_denied", "YouTube authorization was denied.")
		return
	}
	state := strings.TrimSpace(query.Get("state"))
	code := strings.TrimSpace(query.Get("code"))
	if state == "" || code == "" {
		h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid YouTube callback request.")
		return
	}
	channel, err := h.youtube.CompleteCallback(r.Context(), state, code)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidOAuthState):
			h.writeError(w, r, http.StatusBadRequest, "invalid_oauth_state", "The connection request is invalid or expired. Start the connection again.")
		case errors.Is(err, ErrUserInactive):
			h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		case errors.Is(err, ErrYouTubeChannelMissing):
			h.writeError(w, r, http.StatusUnprocessableEntity, "youtube_channel_missing", "The Google account has no YouTube channel.")
		case errors.Is(err, ErrYouTubeTokenExchange):
			h.writeError(w, r, http.StatusBadGateway, "youtube_token_exchange_failed", "YouTube authorization could not be completed.")
		default:
			h.logger.Error("YouTube callback failed", "request_id", tokenRequestID(r), "error", err)
			h.writeError(w, r, http.StatusInternalServerError, "internal_error", "An unexpected server error occurred.")
		}
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{
		"connected": true,
		"provider":  StreamingProviderYouTube,
		"channel":   channel,
	})
}
