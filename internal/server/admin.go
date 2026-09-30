package server

import (
	"net/http"
	"strings"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"

	"github.com/google/uuid"
)

// adminUserSearchLimit는 사용자 검색 한 번에 돌려주는 최대 인원이다.
const adminUserSearchLimit = 50

// registerAdminRoutes는 관리자 전용 라우트를 붙인다(#373). 관리자 목록과 사용자
// 조회가 플랜 저장소에 기대므로 SetPlanStore가 부른다.
func (s *Server) registerAdminRoutes() {
	s.mux.Handle("GET /admin/me", s.requireUser(s.requireAdmin(http.HandlerFunc(s.handleGetAdminMe))))
	s.mux.Handle("GET /admin/users", s.requireUser(s.requireAdmin(http.HandlerFunc(s.handleSearchAdminUsers))))
	if s.sessions != nil {
		s.mux.Handle("GET /admin/sessions", s.requireUser(s.requireAdmin(http.HandlerFunc(s.handleListAdminSessions))))
		s.mux.Handle("DELETE /admin/sessions/{session_id}", s.requireUser(s.requireAdmin(http.HandlerFunc(s.handleDeleteAdminSession))))
	}
}

// requireAdmin은 ADMIN_USER_IDS에 있는 사용자만 통과시킨다. requireUser 안쪽에 둔다.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.adminUser(r); !ok {
			writeAdminForbidden(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// adminUser는 요청한 로그인 사용자가 관리자인지 확인한다. 관리자 핸들러는
// requireAdmin을 거쳐도 이 확인을 다시 한다 — 라우트 배선이 바뀌어 미들웨어가
// 빠지더라도 관리자 기능이 열리지 않게 하기 위해서다.
func (s *Server) adminUser(r *http.Request) (uuid.UUID, bool) {
	userID, authenticated := auth.UserIDFromContext(r.Context())
	if !authenticated || userID == uuid.Nil {
		return uuid.Nil, false
	}
	_, ok := s.admins[userID]
	return userID, ok
}

func writeAdminForbidden(w http.ResponseWriter) {
	writeError(w, apiError{Status: http.StatusForbidden, Code: "forbidden", Message: "Administrator access is required."})
}

// handleGetAdminMe는 테스트 클라이언트의 로그인 게이트가 관리자 여부를 묻는 경로다.
func (s *Server) handleGetAdminMe(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.adminUser(r)
	if !ok {
		writeAdminForbidden(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user_id": userID})
}

type adminSession struct {
	SessionID string `json:"session_id"`
	// UserID·Email은 게스트 세션이면 비어 있다.
	UserID              *uuid.UUID            `json:"user_id"`
	Email               string                `json:"email"`
	Guest               bool                  `json:"guest"`
	Plan                string                `json:"plan"`
	Status              string                `json:"status"`
	BroadcastResolution string                `json:"broadcast_resolution"`
	CreatedAt           time.Time             `json:"created_at"`
	Targets             []session.TargetState `json:"targets"`
}

// handleListAdminSessions는 모든 사용자의 활성 세션을 돌려준다. 배포 게이트가 활성
// 세션 0을 기다리므로, 남은 세션을 찾아 끊는 용도다. owner token은 싣지 않는다.
func (s *Server) handleListAdminSessions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminUser(r); !ok {
		writeAdminForbidden(w)
		return
	}
	live := s.sessions.List()
	ids := make([]uuid.UUID, 0, len(live))
	for _, liveSession := range live {
		if liveSession.UserID != uuid.Nil {
			ids = append(ids, liveSession.UserID)
		}
	}
	users, err := s.plans.UsersByID(r.Context(), ids)
	if err != nil {
		s.logger.Error("admin session list user lookup failed", "error", err)
		writeError(w, internalError())
		return
	}
	emails := make(map[uuid.UUID]string, len(users))
	for _, user := range users {
		emails[user.ID] = user.Email
	}
	result := make([]adminSession, 0, len(live))
	for _, liveSession := range live {
		response := liveSession.Response()
		item := adminSession{
			SessionID:           liveSession.ID,
			Guest:               liveSession.UserID == uuid.Nil,
			Plan:                string(liveSession.Plan),
			Status:              response.Status,
			BroadcastResolution: response.BroadcastResolution,
			CreatedAt:           response.CreatedAt,
			Targets:             response.Targets,
		}
		if !item.Guest {
			userID := liveSession.UserID
			item.UserID = &userID
			item.Email = emails[userID]
		}
		if item.Targets == nil {
			item.Targets = []session.TargetState{}
		}
		result = append(result, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": result})
}

// handleDeleteAdminSession은 관리자가 세션을 강제로 끝낸다. 소유자의 삭제와 같은
// 정리 경로(Delete)를 타므로 준비된 방송·egress도 함께 정리된다.
func (s *Server) handleDeleteAdminSession(w http.ResponseWriter, r *http.Request) {
	adminID, ok := s.adminUser(r)
	if !ok {
		writeAdminForbidden(w)
		return
	}
	id := r.PathValue("session_id")
	if err := s.sessions.Delete(id, "admin_forced"); err != nil {
		writeSessionError(w, err, id)
		return
	}
	s.logger.Info("session closed by admin", "admin_user_id", adminID, "session_id", id)
	w.WriteHeader(http.StatusNoContent)
}

// handleSearchAdminUsers는 이메일로 사용자를 찾는다. 플랜 변경 대상을 고르는 용도다.
func (s *Server) handleSearchAdminUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminUser(r); !ok {
		writeAdminForbidden(w)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("email"))
	users, err := s.plans.SearchUsers(r.Context(), query, adminUserSearchLimit)
	if err != nil {
		s.logger.Error("admin user search failed", "error", err)
		writeError(w, internalError())
		return
	}
	if users == nil {
		users = []auth.AdminUser{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}
