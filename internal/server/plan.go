package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/plan"

	"github.com/google/uuid"
)

// PlanStore는 사용자 요금제 저장소다(#270). 프로덕션은 auth.PlanStore다.
type PlanStore interface {
	UserPlan(ctx context.Context, userID uuid.UUID) (plan.Plan, error)
	SetUserPlan(ctx context.Context, userID uuid.UUID, value plan.Plan) error
	// 아래 둘은 관리자 화면용이다(#373).
	SearchUsers(ctx context.Context, query string, limit int) ([]auth.AdminUser, error)
	UsersByID(ctx context.Context, ids []uuid.UUID) ([]auth.AdminUser, error)
}

// SetPlanStore는 플랜 조회·관리자 지정 라우트를 붙이고, 세션 생성이 소유자
// 플랜을 싣게 한다. Handler가 요청을 받기 전 서버 조립 단계에서 호출한다.
// DB 없이 조립되는 배포(벤치·일부 테스트)는 부르지 않으며, 그때 라우트는 404다.
func (s *Server) SetPlanStore(store PlanStore) {
	if store == nil || s.mux == nil {
		return
	}
	s.plans = store
	s.admins = make(map[uuid.UUID]struct{}, len(s.cfg.AdminUserIDs))
	for _, raw := range s.cfg.AdminUserIDs {
		// config.Validate가 이미 형식을 확인했다.
		if id, err := uuid.Parse(raw); err == nil {
			s.admins[id] = struct{}{}
		}
	}
	s.mux.Handle("GET /users/me/plan", s.requireUser(http.HandlerFunc(s.handleGetMyPlan)))
	s.mux.Handle("PUT /admin/users/{user_id}/plan", s.requireUser(http.HandlerFunc(s.handlePutUserPlan)))
	s.registerAdminRoutes()
	if s.sessions != nil {
		s.sessions.SetPlanResolver(store.UserPlan)
	}
}

type planResponse struct {
	Plan plan.Plan `json:"plan"`
	// 시간 값은 초 단위이며 0은 무제한이다.
	MonthlyBroadcastSeconds int64       `json:"monthly_broadcast_seconds"`
	MaxPerBroadcastSeconds  int64       `json:"max_per_broadcast_seconds"`
	AllowedModes            []plan.Mode `json:"allowed_modes"`
	FaceSlots               int         `json:"face_slots"`
}

func newPlanResponse(value plan.Plan) (planResponse, bool) {
	policy, ok := value.Policy()
	if !ok {
		return planResponse{}, false
	}
	modes := policy.AllowedModes
	if modes == nil {
		modes = []plan.Mode{}
	}
	return planResponse{
		Plan:                    value,
		MonthlyBroadcastSeconds: int64(policy.MonthlyBroadcast.Seconds()),
		MaxPerBroadcastSeconds:  int64(policy.MaxPerBroadcast.Seconds()),
		AllowedModes:            modes,
		FaceSlots:               policy.FaceSlots,
	}, true
}

func (s *Server) handleGetMyPlan(w http.ResponseWriter, r *http.Request) {
	userID, _ := auth.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		writeError(w, apiError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "Authentication is required."})
		return
	}
	value, err := s.plans.UserPlan(r.Context(), userID)
	if err != nil {
		s.logger.Error("read user plan failed", "user_id", userID, "error", err)
		writeError(w, internalError())
		return
	}
	response, ok := newPlanResponse(value)
	if !ok {
		s.logger.Error("user has unknown plan", "user_id", userID, "plan", value)
		writeError(w, internalError())
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// handlePutUserPlan은 관리자가 사용자 플랜을 지정한다. 결제 연동(#277) 전의
// 유일한 플랜 변경 경로다. 진행 중인 세션에는 반영되지 않는다.
func (s *Server) handlePutUserPlan(w http.ResponseWriter, r *http.Request) {
	adminID, _ := auth.UserIDFromContext(r.Context())
	if _, ok := s.admins[adminID]; !ok || adminID == uuid.Nil {
		writeError(w, apiError{Status: http.StatusForbidden, Code: "forbidden", Message: "Only administrators can change plans."})
		return
	}
	targetID, err := uuid.Parse(r.PathValue("user_id"))
	if err != nil {
		writeError(w, badRequest("user_id must be a UUID.", map[string]any{"field": "user_id"}))
		return
	}
	request := struct {
		Plan string `json:"plan"`
	}{}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := decodeOptionalJSON(r.Body, &request); err != nil {
		writeError(w, badRequest("Invalid plan request.", map[string]any{"error": err.Error()}))
		return
	}
	value := plan.Plan(strings.TrimSpace(request.Plan))
	if !value.Valid() {
		writeError(w, badRequest("plan must be one of spark, glow, beam, plasma.", map[string]any{"field": "plan"}))
		return
	}
	if err := s.plans.SetUserPlan(r.Context(), targetID, value); err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			writeError(w, apiError{Status: http.StatusNotFound, Code: "not_found", Message: "User not found.", Details: map[string]any{"user_id": targetID}})
			return
		}
		s.logger.Error("set user plan failed", "user_id", targetID, "error", err)
		writeError(w, internalError())
		return
	}
	s.logger.Info("user plan changed by admin", "admin_user_id", adminID, "user_id", targetID, "plan", value)
	response, _ := newPlanResponse(value)
	writeJSON(w, http.StatusOK, struct {
		UserID uuid.UUID `json:"user_id"`
		planResponse
	}{UserID: targetID, planResponse: response})
}
