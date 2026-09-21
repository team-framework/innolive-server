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
	expired := false
	if err != nil {
		claims, err = h.service.ValidateExpiredAccessToken(raw)
		if err != nil {
			h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
			return
		}
		expired = true
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return
	}
	state, err := h.withdrawal.UserState(r.Context(), userID)
	if err != nil {
		h.logger.Error("account withdrawal state lookup failed", "request_id", tokenRequestID(r), "error", err)
		h.writeError(w, r, http.StatusServiceUnavailable, "withdrawal_unavailable", "Account deletion is temporarily unavailable.")
		return
	}
	switch state {
	case WithdrawalUserMissing:
		w.WriteHeader(http.StatusNoContent)
		return
	case WithdrawalUserInactive:
		h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return
	case WithdrawalUserActive:
		if expired {
			h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
			return
		}
	default:
		h.logger.Error("account withdrawal state lookup returned unknown state", "request_id", tokenRequestID(r))
		h.writeError(w, r, http.StatusServiceUnavailable, "withdrawal_unavailable", "Account deletion is temporarily unavailable.")
		return
	}
	if err := h.withdrawal.Withdraw(r.Context(), userID); err != nil {
		switch {
		case errors.Is(err, ErrWithdrawalInProgress):
			h.writeError(w, r, http.StatusConflict, "withdrawal_in_progress", "Account deletion is already in progress. Retry shortly.")
		case errors.Is(err, ErrUserInactive):
			state, lookupErr := h.withdrawal.UserState(r.Context(), userID)
			if lookupErr != nil {
				h.logger.Error("account withdrawal state lookup failed", "request_id", tokenRequestID(r), "error", lookupErr)
				h.writeError(w, r, http.StatusServiceUnavailable, "withdrawal_unavailable", "Account deletion is temporarily unavailable.")
			} else if state == WithdrawalUserMissing {
				w.WriteHeader(http.StatusNoContent)
			} else {
				h.writeError(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
			}
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
