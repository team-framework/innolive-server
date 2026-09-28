package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/plan"
	"inno-live-server/internal/usage"

	"github.com/google/uuid"
)

// UsageLedger는 사용자의 월 방송 시간 원장이다(#274). 프로덕션은 usage.Ledger다.
type UsageLedger interface {
	Month(ctx context.Context, userID uuid.UUID, from, to, now time.Time) ([]usage.SessionCharge, error)
}

// SetUsageLedger는 사용 내역 라우트를 붙인다. 월 한도가 플랜에서 오므로
// SetPlanStore 뒤에 부른다. 서버 조립 단계에서 한 번만 호출한다.
func (s *Server) SetUsageLedger(ledger UsageLedger) {
	if ledger == nil || s.plans == nil || s.mux == nil {
		return
	}
	s.usageLedger = ledger
	s.mux.Handle("GET /users/me/usage", s.requireUser(http.HandlerFunc(s.handleGetMyUsage)))
}

type usageBroadcastResponse struct {
	SessionID  uuid.UUID `json:"session_id"`
	StartedAt  time.Time `json:"started_at"`
	Resolution string    `json:"resolution"`
	Providers  []string  `json:"providers"`
	// 방송한 시간(하나라도 송출 중인 시간)과 줄어든 방송 시간(유닛-시간)
	BroadcastSeconds int64 `json:"broadcast_seconds"`
	ChargedSeconds   int64 `json:"charged_seconds"`
}

// modeAvailability는 송출 방식 하나로 남은 방송 시간 동안 실제로 몇 초 방송할 수
// 있는지다(#276). 클라이언트가 배수를 계산하지 않도록 서버가 나눠 준다.
type modeAvailability struct {
	Mode       plan.Mode `json:"mode"`
	Multiplier int       `json:"multiplier"`
	// Seconds는 남은 방송 시간 ÷ 배수다. null이면 무제한(Glow)이다.
	Seconds *int64 `json:"seconds"`
	// Allowed가 false면 플랜이 허용하지 않는 방식이다 — 잠금·업그레이드 안내용으로 함께 내보낸다.
	Allowed bool `json:"allowed"`
}

type usageResponse struct {
	Month string `json:"month"`
	Plan  string `json:"plan"`
	// 한도·남은 시간이 null이면 무제한이다(Glow).
	LimitSeconds     *int64                   `json:"limit_seconds"`
	UsedSeconds      int64                    `json:"used_seconds"`
	RemainingSeconds *int64                   `json:"remaining_seconds"`
	Broadcasts       []usageBroadcastResponse `json:"broadcasts"`
	// AvailableByMode는 송출 방식별 실제 방송 가능 시간이다(#276). 월 잔여만 반영한다.
	AvailableByMode []modeAvailability `json:"available_by_mode"`
	// MaxPerBroadcastSeconds는 1회 최대 방송 시간이다. 월 잔여가 많아도 한 번에 이보다
	// 길게 방송할 수 없다. null이면 무제한이다.
	MaxPerBroadcastSeconds *int64 `json:"max_per_broadcast_seconds"`
}

// availableByMode는 남은 방송 시간을 송출 방식별 배수로 나눈다. remaining이 nil이면
// 무제한이다. 네 방식을 모두 내보내고 허용 여부를 표시한다.
func availableByMode(owner plan.Plan, remaining *int64) []modeAvailability {
	modes := make([]modeAvailability, 0, len(plan.Modes))
	for _, mode := range plan.Modes {
		entry := modeAvailability{Mode: mode, Multiplier: mode.Units(), Allowed: owner.Allows(mode)}
		if remaining != nil {
			seconds := *remaining / int64(entry.Multiplier)
			entry.Seconds = &seconds
		}
		modes = append(modes, entry)
	}
	return modes
}

// handleGetMyUsage는 인증된 사용자 자신의 월 사용 내역만 돌려준다. 대상 사용자를
// 고르는 입력이 없으므로 다른 사용자의 내역은 구조적으로 조회할 수 없다.
func (s *Server) handleGetMyUsage(w http.ResponseWriter, r *http.Request) {
	userID, _ := auth.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		writeError(w, apiError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "Authentication is required."})
		return
	}
	now := time.Now()
	reference := now
	if month := strings.TrimSpace(r.URL.Query().Get("month")); month != "" {
		parsed, err := time.ParseInLocation("2006-01", month, usage.LedgerLocation)
		if err != nil {
			writeError(w, badRequest("month must be YYYY-MM.", map[string]any{"field": "month"}))
			return
		}
		reference = parsed
	}
	from, to := usage.MonthRange(reference)

	owner, err := s.plans.UserPlan(r.Context(), userID)
	if err != nil {
		s.logger.Error("read user plan failed", "user_id", userID, "error", err)
		writeError(w, internalError())
		return
	}
	policy, ok := owner.Policy()
	if !ok {
		s.logger.Error("user has unknown plan", "user_id", userID, "plan", owner)
		writeError(w, internalError())
		return
	}
	charges, err := s.usageLedger.Month(r.Context(), userID, from, to, now)
	if err != nil {
		s.logger.Error("read usage ledger failed", "user_id", userID, "error", err)
		writeError(w, internalError())
		return
	}

	response := usageResponse{Month: from.Format("2006-01"), Plan: string(owner), Broadcasts: []usageBroadcastResponse{}}
	for _, charge := range charges {
		response.UsedSeconds += int64(charge.Charged.Seconds())
		response.Broadcasts = append(response.Broadcasts, usageBroadcastResponse{
			SessionID:        charge.SessionID,
			StartedAt:        charge.StartedAt,
			Resolution:       charge.Resolution,
			Providers:        charge.Providers,
			BroadcastSeconds: int64(charge.OnAir.Seconds()),
			ChargedSeconds:   int64(charge.Charged.Seconds()),
		})
	}
	if policy.MonthlyBroadcast > 0 {
		limit := int64(policy.MonthlyBroadcast.Seconds())
		remaining := max(limit-response.UsedSeconds, 0)
		response.LimitSeconds = &limit
		response.RemainingSeconds = &remaining
	}
	if policy.MaxPerBroadcast > 0 {
		perBroadcast := int64(policy.MaxPerBroadcast.Seconds())
		response.MaxPerBroadcastSeconds = &perBroadcast
	}
	response.AvailableByMode = availableByMode(owner, response.RemainingSeconds)
	writeJSON(w, http.StatusOK, response)
}
