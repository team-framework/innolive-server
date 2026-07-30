package auth

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
)

func (h *tokenHTTPHandler) handleWithdrawal(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE, OPTIONS")
		h.writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
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
	if err := h.withdrawal.Withdraw(r.Context(), userID); err != nil {
		switch {
		case errors.Is(err, ErrUserInactive):
			h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		case errors.Is(err, ErrWithdrawalUnavailable):
			h.writeError(w, r, http.StatusServiceUnavailable, "withdrawal_unavailable", "Account deletion is temporarily unavailable.")
		default:
			h.logger.Error("account withdrawal failed", "request_id", tokenRequestID(r), "error", err)
			h.writeError(w, r, http.StatusBadGateway, "withdrawal_failed", "Account deletion could not be completed.")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
