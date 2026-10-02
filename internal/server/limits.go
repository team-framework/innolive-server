package server

import (
	"context"
	"net/http"
	"time"

	"inno-live-server/internal/plan"
	"inno-live-server/internal/session"
	"inno-live-server/internal/usage"

	"github.com/google/uuid"
)

// 방송 한도 집행(#275). 규칙(노션 BM):
//   - 1회 최대(실제 방송 시간, 일시정지 제외): 30분·10분 전 알림, 도달 시 종료
//   - 월 방송 시간 80%·100% 알림. 소진 시 Spark는 종료, 유료는 진행 중 방송을
//     끊지 않고 다음 방송 준비를 막는다(초과 과금 동의는 #277)
//   - 영상 입력이 일정 시간 없으면 종료
const limitCheckInterval = 15 * time.Second

// 알림·종료 코드. 알림은 세션 응답 notices[]로, 종료 사유는 실사용 기록으로 남는다.
const (
	noticeBroadcastLimit30m     = "broadcast_limit_30m"
	noticeBroadcastLimit10m     = "broadcast_limit_10m"
	noticeBroadcastLimitReached = "broadcast_limit_reached"
	noticeMonthlyUsage80        = "monthly_usage_80"
	noticeMonthlyUsage100       = "monthly_usage_100"
	noticeMonthlyLimitReached   = "monthly_limit_reached"
	noticeNoInputStopped        = "no_input_stopped"
)

type limitDecision struct {
	notices    []string
	stopReason string
}

// decideLimits는 한도 판정이다. onAir는 이번 방송의 실제 방송 시간, used는 이번 달
// 누적 차감이다. checkIdle은 멈추지 않은 라이브 대상이 있고 처리 프레임이 한 번이라도
// 있었는지다 — 모두 멈추면 AI 입력도 멈추므로 입력 없음으로 보면 안 된다.
func decideLimits(owner plan.Plan, onAir, used time.Duration, checkIdle bool, idleFor, idleTimeout time.Duration) limitDecision {
	var decision limitDecision
	stop := func(reason string) {
		if decision.stopReason == "" {
			decision.stopReason = reason
		}
	}
	policy, _ := owner.Policy()
	if limit := policy.MaxPerBroadcast; limit > 0 {
		remaining := limit - onAir
		if remaining <= 30*time.Minute {
			decision.notices = append(decision.notices, noticeBroadcastLimit30m)
		}
		if remaining <= 10*time.Minute {
			decision.notices = append(decision.notices, noticeBroadcastLimit10m)
		}
		if remaining <= 0 {
			decision.notices = append(decision.notices, noticeBroadcastLimitReached)
			stop(noticeBroadcastLimitReached)
		}
	}
	if limit := policy.MonthlyBroadcast; limit > 0 {
		if used*5 >= limit*4 {
			decision.notices = append(decision.notices, noticeMonthlyUsage80)
		}
		if used >= limit {
			decision.notices = append(decision.notices, noticeMonthlyUsage100)
			// 유료 플랜은 진행 중 방송을 끊지 않는다 — 다음 방송 준비에서 막는다.
			if owner == plan.Spark {
				decision.notices = append(decision.notices, noticeMonthlyLimitReached)
				stop(noticeMonthlyLimitReached)
			}
		}
	}
	if idleTimeout > 0 && checkIdle && idleFor >= idleTimeout {
		decision.notices = append(decision.notices, noticeNoInputStopped)
		stop(noticeNoInputStopped)
	}
	return decision
}

// broadcastRemaining은 현재 송출 방식(units배)으로 더 방송할 수 있는 시간이다(#276).
// 월 잔여 ÷ 배수와 1회 잔여 중 작은 값이며, 한도가 없으면 nil이다.
func broadcastRemaining(owner plan.Plan, onAir, used time.Duration, units int) *time.Duration {
	policy, _ := owner.Policy()
	var remaining *time.Duration
	consider := func(value time.Duration) {
		value = max(value, 0)
		if remaining == nil || value < *remaining {
			remaining = &value
		}
	}
	if policy.MonthlyBroadcast > 0 && units > 0 {
		consider((policy.MonthlyBroadcast - used) / time.Duration(units))
	}
	if policy.MaxPerBroadcast > 0 {
		consider(policy.MaxPerBroadcast - onAir)
	}
	return remaining
}

// RunLimitEnforcer는 ctx가 끝날 때까지 주기적으로 한도를 집행한다. 원장·플랜
// 저장소가 조립되지 않은 배포(벤치·로컬)에서는 곧바로 돌아온다.
func (s *Server) RunLimitEnforcer(ctx context.Context) {
	if s.usageLedger == nil || s.plans == nil || s.sessions == nil {
		return
	}
	ticker := time.NewTicker(limitCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.enforceLimits(ctx, now)
		}
	}
}

func (s *Server) enforceLimits(ctx context.Context, now time.Time) {
	s.publishYouTubeQuota()
	var upgrades []upgradeCandidate
	defer func() { s.offerUpgrades(upgrades, now) }()
	for _, live := range s.sessions.List() {
		if live.Plan == "" || live.UserID == uuid.Nil {
			continue
		}
		targets, unpaused := live.BroadcastActivity()
		if len(targets) == 0 {
			s.fillIdleBroadcastRemaining(ctx, live, now)
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, limitCheckInterval)
		onAir, used, err := s.broadcastUsage(checkCtx, live.UserID, live.ID, live.CreatedAt, now)
		cancel()
		if err != nil {
			s.logger.Error("limit check failed", "session_id", live.ID, "error", err)
			continue
		}
		idleSince := live.MediaIdleSince()
		checkIdle := unpaused > 0 && !idleSince.IsZero()
		decision := decideLimits(live.Plan, onAir, used, checkIdle, now.Sub(idleSince), s.cfg.BroadcastIdleTimeout)
		live.SetBroadcastRemaining(broadcastRemaining(live.Plan, onAir, used, plan.Units(live.Resolution() == session.ResolutionFHD, len(targets))))
		for _, code := range decision.notices {
			if live.AddNotice(code, now) {
				s.logger.Info("broadcast limit notice", "session_id", live.ID, "plan", live.Plan, "code", code,
					"on_air_seconds", int64(onAir.Seconds()), "used_seconds", int64(used.Seconds()))
			}
		}
		if decision.stopReason == "" {
			s.stopPlatformEndedTargets(ctx, live, targets, now)
			s.noticeYouTubeQuotaLowFor(live, targets, now)
			addable := s.addableProviders(ctx, live, targets)
			if candidate, ok := upgradeCandidateFor(live, targets, addable, onAir, used); ok {
				upgrades = append(upgrades, candidate)
			}
			continue
		}
		for _, provider := range targets {
			_, broadcast, phase, err := s.sessions.StopStreamWithReason(live.ID, decision.stopReason, provider)
			if err != nil {
				s.logger.Warn("limit stop failed", "session_id", live.ID, "provider", provider, "error", err)
				continue
			}
			s.disposeBroadcast(live.UserID, broadcast, phase)
		}
		s.logger.Warn("broadcast stopped by limit", "session_id", live.ID, "plan", live.Plan, "reason", decision.stopReason,
			"on_air_seconds", int64(onAir.Seconds()), "used_seconds", int64(used.Seconds()))
	}
}

// fillIdleBroadcastRemaining은 방송 중이 아닌 세션의 남은 시간을 채운다(#394).
// 송출이 없으면 차감도 없으므로 이미 값이 있으면 그대로 둔다. 처음 채울 때는 송출
// 대상 하나(현재 해상도) 기준이다. 원장·플랜이 없거나 한도가 없는 세션은 건너뛴다.
func (s *Server) fillIdleBroadcastRemaining(ctx context.Context, live *session.Session, now time.Time) {
	if s.usageLedger == nil || live.Plan == "" || live.UserID == uuid.Nil || live.HasBroadcastRemaining() {
		return
	}
	if policy, _ := live.Plan.Policy(); policy.MonthlyBroadcast <= 0 && policy.MaxPerBroadcast <= 0 {
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, limitCheckInterval)
	onAir, used, err := s.broadcastUsage(checkCtx, live.UserID, live.ID, live.CreatedAt, now)
	cancel()
	if err != nil {
		s.logger.Error("broadcast remaining fill failed", "session_id", live.ID, "error", err)
		return
	}
	live.SetBroadcastRemaining(broadcastRemaining(live.Plan, onAir, used, plan.Units(live.Resolution() == session.ResolutionFHD, 1)))
}

// broadcastUsage는 이 세션의 실제 방송 시간과 이번 달 누적 차감을 원장에서 읽는다.
// 이번 방송 시간은 월 경계로 자르지 않도록 세션 시작부터 센다.
func (s *Server) broadcastUsage(ctx context.Context, userID uuid.UUID, sessionID string, sessionStart, now time.Time) (onAir, used time.Duration, err error) {
	since, err := s.usageLedger.Month(ctx, userID, sessionStart, now.Add(time.Second), now)
	if err != nil {
		return 0, 0, err
	}
	for _, charge := range since {
		if charge.SessionID.String() == sessionID {
			onAir = charge.OnAir
		}
	}
	used, err = s.monthlyUsed(ctx, userID, now)
	return onAir, used, err
}

func (s *Server) monthlyUsed(ctx context.Context, userID uuid.UUID, now time.Time) (time.Duration, error) {
	from, to := usage.MonthRange(now)
	charges, err := s.usageLedger.Month(ctx, userID, from, to, now)
	if err != nil {
		return 0, err
	}
	var used time.Duration
	for _, charge := range charges {
		used += charge.Charged
	}
	return used, nil
}

// broadcastLimitError는 1회 최대에 이미 닿은 세션의 방송 준비를 막는다(#275).
// 막지 않으면 잠깐 송출됐다가 다음 점검에서 다시 끊긴다. 1회는 세션 단위다.
func (s *Server) broadcastLimitError(ctx context.Context, owner plan.Plan, userID uuid.UUID, sessionID string, sessionStart time.Time) (*apiError, error) {
	if s.usageLedger == nil || owner == "" || userID == uuid.Nil {
		return nil, nil
	}
	policy, _ := owner.Policy()
	if policy.MaxPerBroadcast <= 0 {
		return nil, nil
	}
	now := time.Now()
	charges, err := s.usageLedger.Month(ctx, userID, sessionStart, now.Add(time.Second), now)
	if err != nil {
		return nil, err
	}
	for _, charge := range charges {
		if charge.SessionID.String() == sessionID && charge.OnAir >= policy.MaxPerBroadcast {
			return &apiError{Status: http.StatusForbidden, Code: noticeBroadcastLimitReached, Message: "This broadcast reached its maximum length. Start a new session.",
				Details: map[string]any{"plan": owner, "on_air_seconds": int64(charge.OnAir.Seconds()), "limit_seconds": int64(policy.MaxPerBroadcast.Seconds())}}, nil
		}
	}
	return nil, nil
}

// monthlyLimitError는 월 방송 시간을 다 쓴 사용자의 다음 방송 준비를 막는다(#275).
// 유료 플랜의 초과 과금 동의는 결제(#277)에서 이 판정에 붙는다.
func (s *Server) monthlyLimitError(ctx context.Context, owner plan.Plan, userID uuid.UUID) (*apiError, error) {
	if s.usageLedger == nil || owner == "" || userID == uuid.Nil {
		return nil, nil
	}
	policy, _ := owner.Policy()
	if policy.MonthlyBroadcast <= 0 {
		return nil, nil
	}
	used, err := s.monthlyUsed(ctx, userID, time.Now())
	if err != nil {
		return nil, err
	}
	if used < policy.MonthlyBroadcast {
		return nil, nil
	}
	return &apiError{Status: http.StatusForbidden, Code: "monthly_limit_exhausted", Message: "This month's broadcast time is used up.",
		Details: map[string]any{"plan": owner, "used_seconds": int64(used.Seconds()), "limit_seconds": int64(policy.MonthlyBroadcast.Seconds())}}, nil
}
