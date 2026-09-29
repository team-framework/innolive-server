package session

import (
	"errors"
	"time"
)

// ErrBroadcastNotLive는 방송 중 설정 변경(#334)을 받을 방송이 없다는 뜻이다.
var ErrBroadcastNotLive = errors.New("broadcast is not prepared or live")

// liveSettingsTargetLocked는 방송 중 설정을 바꿀 수 있는 대상인지 본다. 준비된
// 방송과 라이브 방송만 받는다 — 준비 중에는 저장값을 준비가 읽어 가는 중이다.
func (s *Session) liveSettingsTargetLocked(provider string) error {
	if s.closed {
		return ErrNotFound
	}
	switch s.target(provider).phase {
	case BroadcastPhasePrepared, BroadcastPhaseLive:
		return nil
	default:
		return ErrBroadcastNotLive
	}
}

// CheckLiveSettingsTarget은 플랫폼을 부르기 전에 대상이 방송 중인지 확인한다.
func (m *Manager) CheckLiveSettingsTarget(id, provider string) (*Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.liveSettingsTargetLocked(provider); err != nil {
		return nil, err
	}
	return s, nil
}

// CommitLiveBroadcastSettings는 플랫폼에 반영된 유튜브 설정을 저장한다. 플랫폼
// 반영이 성공한 뒤에만 부른다 — 저장값과 실제 방송이 갈리지 않게 한다.
func (m *Manager) CommitLiveBroadcastSettings(id, provider string, settings YouTubeBroadcastSettings) (*Session, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.liveSettingsTargetLocked(provider); err != nil {
		return nil, err
	}
	settings.UpdatedAt = time.Now().UTC()
	s.broadcast = &settings
	s.UpdatedAt = settings.UpdatedAt
	m.logger.Info("live broadcast settings updated", "session_id", s.ID, "provider", provider)
	return s, nil
}

// CommitLiveChzzkBroadcastSettings는 치지직 설정을 같은 규칙으로 저장한다.
func (m *Manager) CommitLiveChzzkBroadcastSettings(id, provider string, settings ChzzkBroadcastSettings) (*Session, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.liveSettingsTargetLocked(provider); err != nil {
		return nil, err
	}
	settings.UpdatedAt = time.Now().UTC()
	settings.Tags = append([]string(nil), settings.Tags...)
	s.chzzkBroadcast = &settings
	s.UpdatedAt = settings.UpdatedAt
	m.logger.Info("live broadcast settings updated", "session_id", s.ID, "provider", provider)
	return s, nil
}
