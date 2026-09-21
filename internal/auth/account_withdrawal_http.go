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
		claims, retryErr := h.service.ValidateAccessTokenAtExpiry(raw)
		if retryErr == nil {
			if userID, parseErr := uuid.Parse(claims.Subject); parseErr == nil {
				if deleted, lookupErr := h.withdrawal.IsDeleted(r.Context(), userID); lookupErr == nil && deleted {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
		}
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
		case errors.Is(err, ErrWithdrawalInProgress):
			h.writeError(w, r, http.StatusConflict, "withdrawal_in_progress", "Account deletion is already in progress. Retry shortly.")
		case errors.Is(err, ErrUserInactive):
			// The authenticated subject has already been deleted. Treat the repeated
			// request as success so clients can reconcile a lost 204 response.
			w.WriteHeader(http.StatusNoContent)
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
