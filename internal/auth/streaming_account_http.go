package auth

import (
	"net/http"

	"github.com/google/uuid"
)

// authenticatedUserID는 Bearer 액세스 토큰에서 사용자 UUID를 복원한다.
// handleWithdrawal의 인라인 인증 3단계와 동일한 패턴이다 — 사용자 상태(active)
// 확인은 서비스 계층이 담당한다.
func (h *tokenHTTPHandler) authenticatedUserID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw, ok := accessBearerToken(r)
	if !ok {
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return uuid.Nil, false
	}
	claims, err := h.service.ValidateAccessToken(raw)
	if err != nil {
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return uuid.Nil, false
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return uuid.Nil, false
	}
	return userID, true
}

// handleListStreamingAccounts는 사용자의 플랫폼 연결 목록을 돌려준다.
// 플랫폼 중립 엔드포인트다 — 새 플랫폼은 배열 항목으로만 나타난다(#88).
func (h *tokenHTTPHandler) handleListStreamingAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	userID, ok := h.authenticatedUserID(w, r)
	if !ok {
		return
	}
	summaries, err := h.streamingAccounts.List(r.Context(), userID)
	if err != nil {
		if isUnauthorizedStreamingError(err) {
			h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
			return
		}
		h.logger.Error("list streaming accounts failed", "request_id", tokenRequestID(r), "error", err)
		h.writeError(w, r, http.StatusInternalServerError, "internal_error", "An unexpected server error occurred.")
		return
	}
	h.writeJSON(w, http.StatusOK, summaries)
}

// handleDisconnectStreamingAccount는 플랫폼 연결을 해제한다(#88).
// 정리 순서(리소스 삭제→권한 취소→행 삭제)와 실패 허용은 서비스가 보장한다.
func (h *tokenHTTPHandler) handleDisconnectStreamingAccount(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.authenticatedUserID(w, r)
	if !ok {
		return
	}
	provider := StreamingProvider(r.PathValue("provider"))
	err := h.streamingAccounts.Disconnect(r.Context(), userID, provider)
	if err != nil {
		switch {
		case isUnauthorizedStreamingError(err):
			h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		case errorsIsStreamingNotFound(err):
			// 미지 provider 문자열도 "연결 없음"으로 수렴한다 — 저장된 적이
			// 없는 이름이므로 계약상 같은 404다.
			h.writeError(w, r, http.StatusNotFound, "not_found", "No connected streaming account for this provider.")
		default:
			h.logger.Error("disconnect streaming account failed", "request_id", tokenRequestID(r), "provider", provider, "error", err)
			h.writeError(w, r, http.StatusInternalServerError, "internal_error", "An unexpected server error occurred.")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func isUnauthorizedStreamingError(err error) bool {
	return err == ErrUserInactive
}

func errorsIsStreamingNotFound(err error) bool {
	return err == ErrStreamingAccountNotFound
}
