package auth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

const maxChzzkConnectRequestBody = 16 << 10

// handleChzzkConnect는 콜백 페이지가 릴레이한 인가 코드를 받아 치지직 계정
// 연결을 완결한다. 브라우저 리다이렉트만으로는 우리 사용자를 식별할 수 없어
// (우리 인증은 Bearer다) 콜백을 클라이언트가 받고 code만 이 엔드포인트로
// 넘기는 구조다. state는 그 클라이언트가 만들고 대조한다.
func (h *tokenHTTPHandler) handleChzzkConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	raw, ok := accessBearerToken(r)
	if !ok {
		h.logChzzkConnectFailure(r, "missing_bearer_token", nil)
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return
	}
	claims, err := h.service.ValidateAccessToken(raw)
	if err != nil {
		h.logChzzkConnectFailure(r, "invalid_access_token", err)
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		h.logChzzkConnectFailure(r, "invalid_subject", err)
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return
	}
	code, state, err := decodeChzzkConnectRequest(w, r)
	if err != nil {
		h.logChzzkConnectFailure(r, "invalid_request", err)
		h.writeError(w, r, http.StatusBadRequest, "bad_request", "Invalid Chzzk connect request.")
		return
	}
	channel, err := h.chzzk.ConnectWithAuthCode(r.Context(), userID, code, state)
	if err != nil {
		switch {
		case errors.Is(err, ErrWithdrawalInProgress):
			h.logChzzkConnectFailure(r, "withdrawal_in_progress", err)
			h.writeError(w, r, http.StatusConflict, "withdrawal_in_progress", "Account deletion is already in progress. Retry shortly.")
		case errors.Is(err, ErrUserInactive):
			h.logChzzkConnectFailure(r, "user_inactive", err)
			h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		case errors.Is(err, ErrChzzkAuthCodeRejected):
			h.logChzzkConnectFailure(r, "auth_code_rejected", err)
			h.writeError(w, r, http.StatusBadRequest, "invalid_auth_code", "The authorization code was rejected. Connect the Chzzk account again.")
		case errors.Is(err, ErrChzzkScopeMissing):
			// 어떤 권한이 빠졌는지는 사용자가 동의 화면에서 다시 골라야 하는
			// 정보라 메시지에 담지 않고 로그로 남긴다.
			h.logChzzkConnectFailure(r, "scope_missing", err)
			h.writeError(w, r, http.StatusUnprocessableEntity, "chzzk_scope_missing", "All requested Chzzk permissions must be granted.")
		case errors.Is(err, ErrChzzkChannelMissing):
			h.logChzzkConnectFailure(r, "channel_missing", err)
			h.writeError(w, r, http.StatusUnprocessableEntity, "chzzk_channel_missing", "The Chzzk account has no channel.")
		case errors.Is(err, ErrChzzkTokenExchange), errors.Is(err, ErrChzzkPlatformUnavailable):
			// 서버↔치지직 통신 실패다. 우리 결함이 아니므로 502로 답하고,
			// 클라이언트에는 뭉뚱그린 문구만 가므로 원인은 이 로그에만 남는다.
			h.logger.Error("Chzzk platform request failed", "request_id", tokenRequestID(r), "error", err)
			h.writeError(w, r, http.StatusBadGateway, "chzzk_token_exchange_failed", "Chzzk authorization could not be completed.")
		default:
			h.logger.Error("Chzzk connect failed", "request_id", tokenRequestID(r), "error", err)
			h.writeError(w, r, http.StatusInternalServerError, "internal_error", "An unexpected server error occurred.")
		}
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{
		"connected": true,
		"provider":  StreamingProviderChzzk,
		"channel":   channel,
	})
}

// logChzzkConnectFailure는 연동 실패를 한 형식으로 남긴다. code·state·token은
// 어떤 경우에도 인자로 넘기지 않는다.
func (h *tokenHTTPHandler) logChzzkConnectFailure(r *http.Request, reason string, err error, attrs ...any) {
	args := []any{"request_id", tokenRequestID(r), "reason", reason}
	args = append(args, attrs...)
	if err != nil {
		args = append(args, "error", err)
	}
	h.logger.Warn("Chzzk connect rejected", args...)
}

func decodeChzzkConnectRequest(w http.ResponseWriter, r *http.Request) (string, string, error) {
	request := struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}{}
	r.Body = http.MaxBytesReader(w, r.Body, maxChzzkConnectRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return "", "", err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", "", errors.New("request body must contain one JSON object")
	}
	request.Code = strings.TrimSpace(request.Code)
	if request.Code == "" {
		return "", "", errors.New("code is required")
	}
	// state는 클라이언트가 만들고 대조한다. 교환 요청에 그대로 실어야 하므로
	// 받아만 두고 서버는 판정하지 않는다.
	return request.Code, strings.TrimSpace(request.State), nil
}

// handleChzzkConfig는 클라이언트가 인가 페이지로 보내는 데 필요한 공개
// 설정을 준다. client_id·redirect_uri는 인가 URL에 그대로 실리는 공개 값이라
// 인증이 필요 없다. client_secret은 포함하지 않는다.
func (h *tokenHTTPHandler) handleChzzkConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	payload := map[string]any{
		"client_id":    h.chzzk.ClientID(),
		"redirect_uri": h.chzzk.RedirectURI(),
		"scopes":       ChzzkRequiredScopes,
	}
	// state를 준 요청에는 조립된 인가 URL도 함께 준다 — 클라이언트가 쿼리를
	// 직접 짜맞추다 redirect_uri를 어긋나게 쓰는 사고를 줄인다.
	if state := strings.TrimSpace(r.URL.Query().Get("state")); state != "" {
		payload["authorize_url"] = h.chzzk.AuthorizeURL(state)
	}
	h.writeJSON(w, http.StatusOK, payload)
}
