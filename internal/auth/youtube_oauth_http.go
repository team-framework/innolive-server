package auth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

const maxYouTubeConnectRequestBody = 16 << 10

// handleYouTubeConnect는 네이티브 클라이언트(GoogleSignIn SDK)가 획득한
// serverAuthCode를 받아 YouTube 계정 연결을 완결한다. 브라우저 리다이렉트
// 없이 XHR 1회로 끝나므로 콜백·state가 없고, 사용자 바인딩은 Bearer 인증이
// 담당한다.
func (h *tokenHTTPHandler) handleYouTubeConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	raw, ok := accessBearerToken(r)
	if !ok {
		h.logYouTubeConnectFailure(r, "missing_bearer_token", nil)
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return
	}
	claims, err := h.service.ValidateAccessToken(raw)
	if err != nil {
		h.logYouTubeConnectFailure(r, "invalid_access_token", err)
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		h.logYouTubeConnectFailure(r, "invalid_subject", err)
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return
	}
	code, source, err := decodeYouTubeConnectRequest(w, r)
	if err != nil {
		h.logYouTubeConnectFailure(r, "invalid_request", err)
		h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid YouTube connect request.")
		return
	}
	channel, err := h.youtube.ConnectWithAuthCode(r.Context(), userID, code, source)
	if err != nil {
		switch {
		case errors.Is(err, ErrUserInactive):
			h.logYouTubeConnectFailure(r, "user_inactive", err, "code_source", source)
			h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		case errors.Is(err, ErrYouTubeAuthCodeRejected):
			h.logYouTubeConnectFailure(r, "auth_code_rejected", err, "code_source", source)
			h.writeError(w, r, http.StatusBadRequest, "invalid_auth_code", "The authorization code was rejected. Sign in with Google again.")
		case errors.Is(err, ErrYouTubeChannelMissing):
			h.logYouTubeConnectFailure(r, "channel_missing", err, "code_source", source)
			h.writeError(w, r, http.StatusUnprocessableEntity, "youtube_channel_missing", "The Google account has no YouTube channel.")
		case errors.Is(err, ErrYouTubeTokenExchange):
			// 서버↔Google 통신 실패다. 클라이언트에는 뭉뚱그린 문구만 가므로
			// 원인은 이 로그에만 남는다.
			h.logger.Error("YouTube token exchange failed", "request_id", tokenRequestID(r), "code_source", source, "error", err)
			h.writeError(w, r, http.StatusBadGateway, "youtube_token_exchange_failed", "YouTube authorization could not be completed.")
		default:
			h.logger.Error("YouTube connect failed", "request_id", tokenRequestID(r), "code_source", source, "error", err)
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

// logYouTubeConnectFailure는 연동 실패를 한 형식으로 남긴다. 실패 대부분은
// 사용자 입력·인증 문제라 WARN이다. server_auth_code는 자격증명이므로
// 어떤 경우에도 인자로 넘기지 않는다.
func (h *tokenHTTPHandler) logYouTubeConnectFailure(r *http.Request, reason string, err error, attrs ...any) {
	args := []any{"request_id", tokenRequestID(r), "reason", reason}
	args = append(args, attrs...)
	if err != nil {
		args = append(args, "error", err)
	}
	h.logger.Warn("YouTube connect rejected", args...)
}

func decodeYouTubeConnectRequest(w http.ResponseWriter, r *http.Request) (string, CodeSource, error) {
	request := struct {
		ServerAuthCode string `json:"server_auth_code"`
		CodeSource     string `json:"code_source"`
	}{}
	r.Body = http.MaxBytesReader(w, r.Body, maxYouTubeConnectRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return "", "", err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", "", errors.New("request body must contain one JSON object")
	}
	request.ServerAuthCode = strings.TrimSpace(request.ServerAuthCode)
	if request.ServerAuthCode == "" {
		return "", "", errors.New("server_auth_code is required")
	}
	// code_source는 선택 필드다 — 생략 시 native(기존 계약 하위호환).
	source := CodeSource(strings.TrimSpace(request.CodeSource))
	if source == "" {
		source = CodeSourceNative
	}
	if !source.Valid() {
		return "", "", errors.New("code_source must be native or web_popup")
	}
	return request.ServerAuthCode, source, nil
}

// handleYouTubeConfig는 웹 클라이언트가 GIS 팝업을 초기화하는 데 필요한
// 공개 설정을 준다. client_id는 공개 식별자라 인증이 필요 없다.
func (h *tokenHTTPHandler) handleYouTubeConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]string{
		"web_client_id": h.youtube.WebClientID(),
		"scope":         YouTubeStreamingScope,
	})
}
