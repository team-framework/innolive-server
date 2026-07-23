package server

import (
	"errors"
	"net/http"
	"strings"

	"inno-live-server/internal/session"
)

// sessionHandler is an HTTP handler that has already been authenticated to a
// specific session owner. The resolved session is passed in so the handler
// never re-fetches it from the manager, which keeps the ownership check and the
// state change on the same *Session instance (avoids a TOCTOU re-lookup).
type sessionHandler func(w http.ResponseWriter, r *http.Request, sess *session.Session)

// requireSessionOwner wraps a session-scoped handler with owner-token
// verification. Every route under /sessions/{session_id} is registered through
// this wrapper, so a session-mutating route cannot ship without the auth check.
//
//   - missing/malformed Authorization header -> 401
//   - unknown session_id                     -> 404
//   - known session, wrong token             -> 403
//
// Because session_id is a 122-bit random UUID, splitting 404 from 403 does not
// give an attacker a usable enumeration oracle.
func (s *Server) requireSessionOwner(next sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("session_id")
		token := ""
		if s.cfg.RequireSessionAuth {
			bearer, ok := bearerToken(r)
			if !ok {
				writeError(w, apiError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "Missing or malformed Authorization header."})
				return
			}
			token = bearer
		}
		sess, err := s.sessions.VerifyOwner(id, token)
		if errors.Is(err, session.ErrNotFound) {
			writeError(w, apiError{Status: http.StatusNotFound, Code: "not_found", Message: "Session not found.", Details: map[string]any{"session_id": id}})
			return
		}
		if errors.Is(err, session.ErrUnauthorized) {
			writeError(w, apiError{Status: http.StatusForbidden, Code: "forbidden", Message: "Session owner token is invalid.", Details: map[string]any{"session_id": id}})
			return
		}
		if err != nil {
			writeError(w, internalError())
			return
		}
		next(w, r, sess)
	}
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
