package server

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"inno-live-server/internal/plan"
	"inno-live-server/internal/session"
)

const (
	// upgradeOfferTTL은 제안과 유닛 보류가 유지되는 시간이다(#278).
	upgradeOfferTTL = 30 * time.Second
	// upgradeMinRemaining은 새 배수로 이만큼도 방송할 수 없으면 제안하지 않는
	// 기준이다. 올리자마자 한도에 닿으면 새 방송을 여는 비용만 치른다.
	upgradeMinRemaining = time.Hour
)

// upgradeCandidate는 화질을 올릴 수 있는 방송이다.
type upgradeCandidate struct {
	live      *session.Session
	mode      plan.Mode
	unitsFrom int
	unitsTo   int
	remaining *time.Duration
}

// upgradeCandidateFor는 플랜이 허용하는 최고 화질보다 낮게 방송 중인 세션을 고른다.
// 대상 구성은 그대로 두고 해상도만 올린다(720p → FHD, 항상 +1유닛).
func upgradeCandidateFor(live *session.Session, targets int, onAir, used time.Duration) (upgradeCandidate, bool) {
	if targets == 0 || live.Resolution() == session.ResolutionFHD || live.ResolutionSwitching() || live.UpgradeOffered() {
		return upgradeCandidate{}, false
	}
	// 준비·라이브 전환 중인 대상이 있으면 구성이 아직 정해지지 않았다. 지금 세면
	// 동시 송출을 단독으로 잘못 제안한다 — 다음 점검에서 본다(#329).
	if live.BusyTargetCount() != targets {
		return upgradeCandidate{}, false
	}
	if live.Plan != plan.Beam && live.Plan != plan.Plasma {
		return upgradeCandidate{}, false
	}
	mode := plan.ModeFor(true, targets)
	if !live.Plan.Allows(mode) {
		return upgradeCandidate{}, false
	}
	unitsTo := plan.Units(true, targets)
	remaining := broadcastRemaining(live.Plan, onAir, used, unitsTo)
	if remaining != nil && *remaining < upgradeMinRemaining {
		return upgradeCandidate{}, false
	}
	return upgradeCandidate{live: live, mode: mode, unitsFrom: plan.Units(false, targets), unitsTo: unitsTo, remaining: remaining}, true
}

// offerUpgrades는 우선순위대로 제안을 건다: 플랜(Plasma → Beam), 그다음 먼저 시작한
// 방송. 제안마다 늘어날 유닛을 보류하므로, 자리가 모자라면 그 후보는 건너뛴다.
func (s *Server) offerUpgrades(candidates []upgradeCandidate, now time.Time) {
	slices.SortStableFunc(candidates, func(a, b upgradeCandidate) int {
		if rank := planRank(b.live.Plan) - planRank(a.live.Plan); rank != 0 {
			return rank
		}
		return a.live.CreatedAt.Compare(b.live.CreatedAt)
	})
	for _, candidate := range candidates {
		offer := session.UpgradeOffer{
			Resolution: session.ResolutionFHD,
			Mode:       candidate.mode,
			UnitsFrom:  candidate.unitsFrom,
			UnitsTo:    candidate.unitsTo,
			ExpiresAt:  now.Add(upgradeOfferTTL).UTC(),
		}
		if candidate.remaining != nil {
			seconds := int64(candidate.remaining.Seconds())
			offer.RemainingSecondsAfter = &seconds
		}
		if err := s.sessions.OfferUpgrade(candidate.live.ID, offer, upgradeOfferTTL); err != nil {
			continue
		}
		s.logger.Info("upgrade offered", "session_id", candidate.live.ID, "plan", candidate.live.Plan,
			"mode", candidate.mode, "units_from", candidate.unitsFrom, "units_to", candidate.unitsTo)
	}
}

func planRank(value plan.Plan) int {
	switch value {
	case plan.Plasma:
		return 2
	case plan.Beam:
		return 1
	default:
		return 0
	}
}

// handleDeleteUpgradeOffer는 화질 올리기 제안을 거절한다. 보류한 유닛을 바로
// 돌려주고, 같은 세션에는 다시 제안하지 않는다.
func (s *Server) handleDeleteUpgradeOffer(w http.ResponseWriter, r *http.Request, liveSession *session.Session) {
	_, err := s.sessions.DeclineUpgradeOffer(liveSession.ID)
	switch {
	case errors.Is(err, session.ErrNoUpgradeOffer):
		writeError(w, apiError{Status: http.StatusNotFound, Code: "upgrade_offer_not_found", Message: "There is no pending upgrade offer for this session."})
		return
	case err != nil:
		writeSessionError(w, err, liveSession.ID)
		return
	}
	s.logger.Info("upgrade offer declined", "session_id", liveSession.ID)
	writeJSON(w, http.StatusOK, liveSession.Response())
}
