package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sort"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/plan"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"
)

const (
	// upgradeOfferTTL은 제안과 유닛 보류가 유지되는 시간이다(#278).
	upgradeOfferTTL = 30 * time.Second
	// upgradeSelectTTL은 선택지를 고른 뒤 확정까지 보류를 유지하는 시간이다(#333).
	// 플랫폼을 추가하는 선택지는 새 플랫폼의 방송 설정을 채워야 한다.
	upgradeSelectTTL = 3 * time.Minute
	// upgradeMinRemaining은 새 배수로 이만큼도 방송할 수 없으면 제안하지 않는
	// 기준이다. 올리자마자 한도에 닿으면 새 방송을 여는 비용만 치른다.
	upgradeMinRemaining = time.Hour
	// chzzkRestartGapSeconds는 치지직 재시작 때 이전 방송이 닫힐 때까지의 공백이다
	// (실측 13~14초).
	chzzkRestartGapSeconds = 15
)

// upgradeCandidate는 송출 방식을 올릴 수 있는 방송과 그 선택지다.
type upgradeCandidate struct {
	live      *session.Session
	unitsFrom int
	options   []session.UpgradeOption
}

// upgradeCandidateFor는 플랜이 허용하는 방식 중 지금보다 유닛이 많고 해상도·대상
// 수 어느 쪽도 줄지 않는 방식을 모두 선택지로 만든다(#333). addable은 추가할 수
// 있는(계정이 연결된) 플랫폼이다 — 없으면 동시 송출 선택지는 빠진다.
func upgradeCandidateFor(live *session.Session, targets []string, addable []string, onAir, used time.Duration) (upgradeCandidate, bool) {
	if len(targets) == 0 || live.ResolutionSwitching() || live.UpgradeOffered() {
		return upgradeCandidate{}, false
	}
	// 준비·라이브 전환 중인 대상이 있으면 구성이 아직 정해지지 않았다. 지금 세면
	// 동시 송출을 단독으로 잘못 제안한다 — 다음 점검에서 본다(#329).
	if live.BusyTargetCount() != len(targets) {
		return upgradeCandidate{}, false
	}
	if live.Plan != plan.Beam && live.Plan != plan.Plasma {
		return upgradeCandidate{}, false
	}
	fhd := live.Resolution() == session.ResolutionFHD
	unitsFrom := plan.Units(fhd, len(targets))
	var options []session.UpgradeOption
	for _, mode := range plan.Modes {
		toFHD := mode == plan.ModeFHDSingle || mode == plan.ModeFHDMulti
		toMulti := mode == plan.Mode720pMulti || mode == plan.ModeFHDMulti
		if !live.Plan.Allows(mode) || (fhd && !toFHD) || (len(targets) > 1 && !toMulti) {
			continue
		}
		to := slices.Clone(targets)
		needsSettings := false
		if toMulti && len(targets) == 1 {
			if len(addable) == 0 {
				continue
			}
			to = append(to, addable[0])
			needsSettings = true
		}
		unitsTo := plan.Units(toFHD, len(to))
		if unitsTo <= unitsFrom {
			continue
		}
		remaining := broadcastRemaining(live.Plan, onAir, used, unitsTo)
		if remaining != nil && *remaining < upgradeMinRemaining {
			continue
		}
		resolution := session.Resolution720p
		if toFHD {
			resolution = session.ResolutionFHD
		}
		option := session.UpgradeOption{Mode: mode, Resolution: resolution, Targets: to, UnitsTo: unitsTo, NeedsSettings: needsSettings}
		if remaining != nil {
			seconds := int64(remaining.Seconds())
			option.RemainingSecondsAfter = &seconds
		}
		// 해상도가 바뀌면 플랫폼이 같은 방송 안의 변경을 반영하지 않아 모든 대상을
		// 새 방송으로 다시 연다(#290). 해상도가 같으면 기존 대상은 그대로다.
		if toFHD != fhd {
			option.RestartsBroadcast = true
			option.RestartEffects = restartEffects(targets)
		}
		options = append(options, option)
	}
	if len(options) == 0 {
		return upgradeCandidate{}, false
	}
	return upgradeCandidate{live: live, unitsFrom: unitsFrom, options: options}, true
}

func restartEffects(targets []string) []session.RestartEffect {
	effects := make([]session.RestartEffect, 0, len(targets))
	for _, provider := range targets {
		effect := session.RestartEffect{Provider: provider}
		if auth.StreamingProvider(provider) == auth.StreamingProviderChzzk {
			effect.SameLink = true
			effect.GapSeconds = chzzkRestartGapSeconds
		}
		effects = append(effects, effect)
	}
	return effects
}

// addableProviders는 지금 대상에 더할 수 있는 플랫폼이다 — 서버가 지원하고 사용자
// 계정이 연결된 것만. 이미 제안한 세션은 조회하지 않는다.
func (s *Server) addableProviders(ctx context.Context, live *session.Session, targets []string) []string {
	if live.UpgradeOffered() || len(targets) != 1 {
		return nil
	}
	var names []string
	for provider := range s.streaming {
		if !slices.Contains(targets, string(provider)) {
			names = append(names, string(provider))
		}
	}
	sort.Strings(names)
	var addable []string
	for _, name := range names {
		checker, ok := s.streaming[auth.StreamingProvider(name)].(streaming.ConnectionChecker)
		if !ok {
			continue
		}
		connected, err := checker.Connected(ctx, live.UserID)
		if err != nil {
			s.logger.Warn("upgrade offer connection check failed", "session_id", live.ID, "provider", name, "error", err)
			continue
		}
		if connected {
			addable = append(addable, name)
		}
	}
	return addable
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
			UnitsFrom: candidate.unitsFrom,
			Options:   candidate.options,
			ExpiresAt: now.Add(upgradeOfferTTL).UTC(),
		}
		if err := s.sessions.OfferUpgrade(candidate.live.ID, offer, upgradeOfferTTL); err != nil {
			continue
		}
		shown := candidate.live.Response().UpgradeOffer
		if shown == nil {
			continue
		}
		modes := make([]plan.Mode, 0, len(shown.Options))
		for _, option := range shown.Options {
			modes = append(modes, option.Mode)
		}
		s.logger.Info("upgrade offered", "session_id", candidate.live.ID, "plan", candidate.live.Plan,
			"options", modes, "units_from", candidate.unitsFrom, "held_units", s.sessions.EgressSlots().Held(candidate.live.ID))
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

// handleSelectUpgradeOption은 선택지를 골랐음을 알리고 보류를 늘린다(#333). 확정은
// 기존 송출 방식 전환(PUT /broadcast-mode)이다.
func (s *Server) handleSelectUpgradeOption(w http.ResponseWriter, r *http.Request, liveSession *session.Session) {
	var request struct {
		Mode plan.Mode `json:"mode"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := decodeOptionalJSON(r.Body, &request); err != nil || request.Mode == "" {
		writeError(w, badRequest("mode is required.", nil))
		return
	}
	selected, err := s.sessions.SelectUpgradeOption(liveSession.ID, request.Mode, upgradeSelectTTL)
	switch {
	case errors.Is(err, session.ErrNoUpgradeOffer):
		writeError(w, upgradeOfferNotFound())
		return
	case errors.Is(err, session.ErrUpgradeOptionNotFound):
		writeError(w, apiError{Status: http.StatusBadRequest, Code: "upgrade_option_not_found", Message: "The upgrade offer has no such option.", Details: map[string]any{"mode": request.Mode}})
		return
	case err != nil:
		writeSessionError(w, err, liveSession.ID)
		return
	}
	s.logger.Info("upgrade option selected", "session_id", liveSession.ID, "mode", request.Mode)
	writeJSON(w, http.StatusOK, selected.Response())
}

// handleDeleteUpgradeOffer는 화질 올리기 제안을 거절한다. 보류한 유닛을 바로
// 돌려주고, 같은 세션에는 다시 제안하지 않는다.
func (s *Server) handleDeleteUpgradeOffer(w http.ResponseWriter, r *http.Request, liveSession *session.Session) {
	_, err := s.sessions.DeclineUpgradeOffer(liveSession.ID)
	switch {
	case errors.Is(err, session.ErrNoUpgradeOffer):
		writeError(w, upgradeOfferNotFound())
		return
	case err != nil:
		writeSessionError(w, err, liveSession.ID)
		return
	}
	s.logger.Info("upgrade offer declined", "session_id", liveSession.ID)
	writeJSON(w, http.StatusOK, liveSession.Response())
}

func upgradeOfferNotFound() apiError {
	return apiError{Status: http.StatusNotFound, Code: "upgrade_offer_not_found", Message: "There is no pending upgrade offer for this session."}
}
