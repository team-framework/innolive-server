package session

import (
	"errors"
	"slices"
	"time"

	"inno-live-server/internal/media"
	"inno-live-server/internal/plan"
)

var (
	// ErrUpgradeNotOfferable는 이 세션에 지금 화질 올리기를 제안할 수 없다는 뜻이다
	// (이미 제안함·전환 중·세션 종료).
	ErrUpgradeNotOfferable = errors.New("upgrade offer not available for this session")
	// ErrNoUpgradeOffer는 거절할 제안이 없다는 뜻이다.
	ErrNoUpgradeOffer = errors.New("no pending upgrade offer")
	// ErrUpgradeOptionNotFound는 제안에 없는 선택지를 골랐다는 뜻이다.
	ErrUpgradeOptionNotFound = errors.New("upgrade option not in the offer")
)

// RestartEffect는 방송 재시작이 대상 하나에 주는 영향이다(#333). 확인된 사실만
// 싣는다 — 유튜브는 새 방송이라 링크가 바뀌고, 치지직은 채널 주소가 같지만 이전
// 방송이 닫힐 때까지 공백이 생긴다.
type RestartEffect struct {
	Provider   string `json:"provider"`
	SameLink   bool   `json:"same_link"`
	GapSeconds int    `json:"gap_seconds,omitempty"`
}

// UpgradeOption은 제안의 선택지 하나다.
type UpgradeOption struct {
	Mode                  plan.Mode       `json:"mode"`
	Resolution            string          `json:"resolution"`
	Targets               []string        `json:"targets"`
	UnitsTo               int             `json:"units_to"`
	RemainingSecondsAfter *int64          `json:"remaining_seconds_after"`
	NeedsSettings         bool            `json:"needs_settings"`
	RestartsBroadcast     bool            `json:"restarts_broadcast"`
	RestartEffects        []RestartEffect `json:"restart_effects"`
}

// UpgradeOffer는 빈자리로 송출 방식을 올릴 수 있다는 제안이다(#278, #333). 선택지
// 중 가장 큰 것만큼 유닛을 예산에 보류해 두고, 승낙은 기존 송출 방식 전환(PUT
// /broadcast-mode)으로 한다. 하나를 승낙하면 제안 전체가 사라진다.
//
// Resolution·Mode·UnitsTo·RemainingSecondsAfter는 #278 계약 호환용으로 첫 선택지를
// 그대로 옮긴다.
type UpgradeOffer struct {
	Resolution            string          `json:"resolution"`
	Mode                  plan.Mode       `json:"mode"`
	UnitsFrom             int             `json:"units_from"`
	UnitsTo               int             `json:"units_to"`
	RemainingSecondsAfter *int64          `json:"remaining_seconds_after"`
	Options               []UpgradeOption `json:"options"`
	Selected              plan.Mode       `json:"selected,omitempty"`
	ExpiresAt             time.Time       `json:"expires_at"`
}

// OfferUpgrade는 세션에 제안을 걸고 늘어날 유닛을 보류한다. 빈자리에 들어가는 가장
// 큰 선택지만큼 보류하고, 보류 안에 드는 선택지만 남긴다. 한 세션에는 한 번만
// 제안한다 — 승낙하면 새 방송을 여는데 유튜브 방송 생성에 채널별 한도가 있다.
// 어떤 선택지도 들어가지 않으면 예산 오류를 돌려준다.
func (m *Manager) OfferUpgrade(id string, offer UpgradeOffer, ttl time.Duration) error {
	s, err := m.Get(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.upgradeOffered || len(offer.Options) == 0 || (s.resolutionSwitch != nil && s.resolutionSwitch.Status == ResolutionSwitchSwitching) {
		return ErrUpgradeNotOfferable
	}
	options := slices.Clone(offer.Options)
	slices.SortStableFunc(options, func(a, b UpgradeOption) int { return b.UnitsTo - a.UnitsTo })
	claim := media.EgressClaim{Owner: s.ID, Tier: egressTierFor(s.Plan), Group: egressGroupFor(s.Plan)}
	var holdErr error
	held := 0
	for _, option := range options {
		if holdErr = m.egressSlots.Hold(claim, option.UnitsTo-offer.UnitsFrom); holdErr == nil {
			held = option.UnitsTo - offer.UnitsFrom
			break
		}
	}
	if holdErr != nil {
		return holdErr
	}
	m.publishEgressSlots()
	pending := offer
	pending.Options = slices.DeleteFunc(slices.Clone(offer.Options), func(option UpgradeOption) bool {
		return option.UnitsTo-offer.UnitsFrom > held
	})
	first := pending.Options[0]
	pending.Resolution, pending.Mode, pending.UnitsTo, pending.RemainingSecondsAfter = first.Resolution, first.Mode, first.UnitsTo, first.RemainingSecondsAfter
	s.upgradeOffer = &pending
	s.upgradeOffered = true
	s.upgradeTimer = time.AfterFunc(ttl, func() { m.expireUpgradeOffer(id, &pending) })
	s.UpdatedAt = time.Now().UTC()
	return nil
}

// SelectUpgradeOption은 사용자가 선택지를 골랐음을 기록하고 보류를 ttl만큼 늘린다.
// 플랫폼을 추가하는 선택지는 새 플랫폼의 방송 설정을 채워야 해 제안 기본 시간으로는
// 모자란다. 확정하지 않으면 늘린 시간이 지나 반납된다.
func (m *Manager) SelectUpgradeOption(id string, mode plan.Mode, ttl time.Duration) (*Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	offer := s.upgradeOffer
	if offer == nil {
		return nil, ErrNoUpgradeOffer
	}
	if !slices.ContainsFunc(offer.Options, func(option UpgradeOption) bool { return option.Mode == mode }) {
		return nil, ErrUpgradeOptionNotFound
	}
	if s.upgradeTimer != nil {
		s.upgradeTimer.Stop()
	}
	offer.Selected = mode
	offer.ExpiresAt = time.Now().Add(ttl).UTC()
	s.upgradeTimer = time.AfterFunc(ttl, func() { m.expireUpgradeOffer(id, offer) })
	s.UpdatedAt = time.Now().UTC()
	return s, nil
}

// DeclineUpgradeOffer는 제안을 거두고 보류를 반납한다. 다시 제안하지 않는다.
func (m *Manager) DeclineUpgradeOffer(id string) (*Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.upgradeOffer == nil {
		s.mu.Unlock()
		return nil, ErrNoUpgradeOffer
	}
	s.clearUpgradeOfferLocked()
	s.mu.Unlock()
	m.ReleaseUpgradeHold(id)
	return s, nil
}

// ReleaseUpgradeHold는 남은 보류분을 반납한다. 전환이 끝났을 때 부른다 — 승낙한
// 전환이 새 송출을 잡으며 이미 소비했으면 아무 일도 없다.
func (m *Manager) ReleaseUpgradeHold(id string) {
	m.egressSlots.ReleaseHold(id)
	m.publishEgressSlots()
}

func (m *Manager) expireUpgradeOffer(id string, offer *UpgradeOffer) {
	s, err := m.Get(id)
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.upgradeOffer != offer {
		s.mu.Unlock()
		return
	}
	s.clearUpgradeOfferLocked()
	s.mu.Unlock()
	held := m.egressSlots.Held(id)
	m.ReleaseUpgradeHold(id)
	m.logger.Info("upgrade offer expired", "session_id", id, "selected", offer.Selected, "released_units", held)
}

// UpgradeOffered는 이 세션에 이미 제안한 적이 있는지다.
func (s *Session) UpgradeOffered() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.upgradeOffered
}

// clearUpgradeOfferLocked는 제안 표시와 만료 타이머를 치운다. 보류는 호출자가
// 정리한다 — 승낙(전환 시작)은 보류를 남겨 새 송출이 쓰게 한다.
func (s *Session) clearUpgradeOfferLocked() {
	if s.upgradeTimer != nil {
		s.upgradeTimer.Stop()
		s.upgradeTimer = nil
	}
	s.upgradeOffer = nil
	s.UpdatedAt = time.Now().UTC()
}

func egressGroupFor(value plan.Plan) string {
	if value == "" {
		return "none"
	}
	return string(value)
}
