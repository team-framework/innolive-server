package server

import (
	"errors"
	"net/http"
	"strings"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"
)

// sessionHandler는 특정 세션 소유자로 이미 인증된 HTTP 핸들러다. 확인한 세션을
// 넘겨받아 매니저에서 다시 찾지 않으므로, 소유권 확인과 상태 변경이 같은 *Session
// 인스턴스에서 일어난다(재조회로 생기는 TOCTOU를 피한다).
type sessionHandler func(w http.ResponseWriter, r *http.Request, sess *session.Session)

// requireSessionOwner는 세션 범위 핸들러를 소유자 토큰 검증으로 감싼다.
// /sessions/{session_id} 아래 모든 경로를 이 래퍼로 등록하므로, 세션을 바꾸는
// 경로가 인증 확인 없이 나갈 수 없다.
//
//   - 소유자 토큰 없음                 -> 401
//   - 모르는 session_id                -> 404
//   - 아는 세션, 틀린 토큰             -> 403
//
// session_id는 122비트 무작위 UUID라 404와 403을 나눠도 공격자가 쓸 만한 열거
// 신호가 되지 않는다.
func (s *Server) requireSessionOwner(next sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("session_id")
		token := strings.TrimSpace(r.Header.Get("X-Session-Owner-Token"))
		if s.cfg.RequireSessionAuth {
			// 사용자 범위 경로는 Authorization을 access token에 쓴다. RequireUser를 달지
			// 않는 로컬·테스트 서버에만 Authorization 대체 경로를 남겨, 인증을 명시적으로
			// 끈 작업 흐름을 유지한다.
			if token == "" {
				if _, authenticated := auth.UserIDFromContext(r.Context()); !authenticated {
					bearer, ok := bearerToken(r)
					if ok {
						token = bearer
					}
				}
			}
			if token == "" {
				writeError(w, apiError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "Session owner token is required."})
				return
			}
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
		if userID, authenticated := auth.UserIDFromContext(r.Context()); authenticated && sess.UserID != userID {
			writeError(w, apiError{Status: http.StatusForbidden, Code: "forbidden", Message: "Session does not belong to the authenticated user.", Details: map[string]any{"session_id": id}})
			return
		}
		if userID, authenticated := auth.UserIDFromContext(r.Context()); authenticated && s.userOperationGate != nil {
			release, admitted := s.userOperationGate.BeginOperation(userID)
			if !admitted {
				writeError(w, apiError{Status: http.StatusConflict, Code: "withdrawal_in_progress", Message: "Account deletion is already in progress. Retry shortly."})
				return
			}
			defer release()
		}
		next(w, r, sess)
	}
}

// bearerToken은 "Authorization: Bearer <token>" 헤더에서 토큰을 꺼낸다.
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
