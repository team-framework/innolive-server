package session

import (
	"errors"
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
)

// UpgradeOffer는 빈자리로 화질을 올릴 수 있다는 제안이다(#278). 제안 동안 늘어날
// 유닛을 예산에 보류해 두고, 승낙은 기존 송출 방식 전환(PUT /broadcast-mode)으로
// 한다.
type UpgradeOffer struct {
	Resolution            string    `json:"resolution"`
	Mode                  plan.Mode `json:"mode"`
	UnitsFrom             int       `json:"units_from"`
	UnitsTo               int       `json:"units_to"`
	RemainingSecondsAfter *int64    `json:"remaining_seconds_after"`
	ExpiresAt             time.Time `json:"expires_at"`
}

// OfferUpgrade는 세션에 제안을 걸고 늘어날 유닛을 보류한다. 한 세션에는 한 번만
// 제안한다 — 승낙하면 새 방송을 여는데 유튜브 방송 생성에 채널별 한도가 있다.
// 자리가 없으면 예산 오류를 그대로 돌려준다.
func (m *Manager) OfferUpgrade(id string, offer UpgradeOffer, ttl time.Duration) error {
	s, err := m.Get(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.upgradeOffered || (s.resolutionSwitch != nil && s.resolutionSwitch.Status == ResolutionSwitchSwitching) {
		return ErrUpgradeNotOfferable
	}
	claim := media.EgressClaim{Owner: s.ID, Tier: egressTierFor(s.Plan), Group: egressGroupFor(s.Plan)}
	if err := m.egressSlots.Hold(claim, offer.UnitsTo-offer.UnitsFrom); err != nil {
		return err
	}
	pending := offer
	s.upgradeOffer = &pending
	s.upgradeOffered = true
	s.upgradeTimer = time.AfterFunc(ttl, func() { m.expireUpgradeOffer(id, &pending) })
	s.UpdatedAt = time.Now().UTC()
	return nil
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
	m.egressSlots.ReleaseHold(id)
	return s, nil
}

// ReleaseUpgradeHold는 남은 보류분을 반납한다. 전환이 끝났을 때 부른다 — 승낙한
// 전환이 새 송출을 잡으며 이미 소비했으면 아무 일도 없다.
func (m *Manager) ReleaseUpgradeHold(id string) {
	m.egressSlots.ReleaseHold(id)
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
	m.egressSlots.ReleaseHold(id)
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
