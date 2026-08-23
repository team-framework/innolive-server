package session

import (
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/media"
)

const (
	defaultWebRTCRecoveryWindow      = 45 * time.Second
	defaultWebRTCRecoveryDebounce    = 2 * time.Second
	defaultWebRTCRecoveryMaxAttempts = 6
)

// peerRecovery는 Session.mu로 보호한다. generation은 이전 disconnected 타이머와
// 늦게 도착한 재협상 요청이 새 복구 구간을 종료시키지 못하게 하는 세대 번호다.
type peerRecovery struct {
	status             PeerRecoveryStatus
	attempts           int
	deadline           *time.Time
	lastError          *string
	generation         uint64
	debounceTimer      *time.Timer
	deadlineTimer      *time.Timer
	awaitingMediaFrame bool
}

func recoveryWindow(cfg config.Config) time.Duration {
	if cfg.WebRTCRecoveryWindow > 0 {
		return cfg.WebRTCRecoveryWindow
	}
	return defaultWebRTCRecoveryWindow
}

func recoveryDebounce(cfg config.Config) time.Duration {
	if cfg.WebRTCRecoveryDebounce > 0 {
		return cfg.WebRTCRecoveryDebounce
	}
	// 기존 테스트·배포가 WEBRTC_DISCONNECTED_GRACE만 지정한 경우에는 그 값을
	// debounce로 이어받는다. 새 설정이 있으면 새 계약이 항상 우선한다.
	if cfg.DisconnectedGracePeriod > 0 {
		return cfg.DisconnectedGracePeriod
	}
	return defaultWebRTCRecoveryDebounce
}

func recoveryMaxAttempts(cfg config.Config) int {
	if cfg.WebRTCRecoveryAttempts > 0 {
		return cfg.WebRTCRecoveryAttempts
	}
	return defaultWebRTCRecoveryMaxAttempts
}

func recoveryError(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

func (m *Manager) scheduleDisconnectedRecovery(s *Session) {
	s.mu.Lock()
	if s.closed || !s.wasConnected || s.recovery.status != PeerRecoveryStatusIdle {
		s.mu.Unlock()
		return
	}
	s.recovery.generation++
	generation := s.recovery.generation
	s.recovery.status = PeerRecoveryStatusWaiting
	s.recovery.lastError = recoveryError("peer_connection_disconnected")
	s.UpdatedAt = time.Now().UTC()
	delay := recoveryDebounce(m.cfg)
	s.recovery.debounceTimer = time.AfterFunc(delay, func() {
		m.beginRecovery(s, generation, "peer_connection_disconnected")
	})
	s.mu.Unlock()
}

// beginRecovery는 연결 상태 콜백·재협상 offer 어느 쪽에서 먼저 호출되어도 같은
// 복구 창을 사용한다. 서버는 offer를 직접 만들 수 없지만, deadline을 보관해
// 백그라운드 전환이나 클라이언트 오류로 세션이 무기한 남지 않게 한다.
func (m *Manager) beginRecovery(s *Session, generation uint64, cause string) {
	s.mu.Lock()
	if s.closed || !s.wasConnected || (generation != 0 && generation != s.recovery.generation) {
		s.mu.Unlock()
		return
	}
	if generation != 0 && s.PC.ConnectionState().String() == "connected" {
		// disconnected debounce가 끝나기 전에 연결이 돌아온 경우다. 새 복구
		// 창을 열지 않고 대기 상태와 타이머만 정리한다.
		m.finishRecoveryLocked(s)
		s.mu.Unlock()
		return
	}
	if s.recovery.status == PeerRecoveryStatusIdle {
		s.recovery.generation++
	}
	if s.recovery.debounceTimer != nil {
		s.recovery.debounceTimer.Stop()
		s.recovery.debounceTimer = nil
	}
	now := time.Now().UTC()
	started := false
	if s.recovery.deadline == nil {
		started = true
		deadline := now.Add(recoveryWindow(m.cfg))
		s.recovery.deadline = &deadline
		generation = s.recovery.generation
		s.recovery.deadlineTimer = time.AfterFunc(time.Until(deadline), func() {
			m.expireRecovery(s.ID, generation)
		})
	}
	s.recovery.status = PeerRecoveryStatusRecovering
	s.recovery.lastError = recoveryError(cause)
	if egress := activeEgressLocked(s); egress != nil {
		egress.BeginInputRecovery()
	}
	s.UpdatedAt = now
	s.mu.Unlock()
	if started {
		m.metrics.IncPeerRecoveryStarted()
		m.logger.Info("WebRTC network recovery started", "session_id", s.ID, "reason", cause)
	}
}

func (m *Manager) expireRecovery(sessionID string, generation uint64) {
	s, err := m.Get(sessionID)
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.closed || s.recovery.generation != generation || s.recovery.status == PeerRecoveryStatusIdle {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	m.metrics.IncPeerRecoveryExhausted()
	m.logger.Warn("WebRTC network recovery exhausted", "session_id", sessionID)
	_ = m.Delete(sessionID, "peer_connection_recovery_exhausted")
}

// registerRecoveryOffer는 클라이언트가 실제로 보낸 ICE restart offer만 센다.
// 클라이언트가 주장하는 횟수는 신뢰하지 않고, 서버가 성공적으로 수락할 offer의
// 상한을 여기서 강제한다.
func (m *Manager) registerRecoveryOffer(s *Session) error {
	m.beginRecovery(s, 0, "ice_restart")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.recovery.status == PeerRecoveryStatusIdle {
		return ErrNotFound
	}
	if s.recovery.attempts >= recoveryMaxAttempts(m.cfg) {
		return ErrRecoveryAttemptsExhausted
	}
	s.recovery.attempts++
	s.UpdatedAt = time.Now().UTC()
	return nil
}

func (m *Manager) connectedDuringRecovery(s *Session) {
	s.mu.Lock()
	if s.closed || s.recovery.status == PeerRecoveryStatusIdle {
		s.mu.Unlock()
		return
	}
	if activeEgressLocked(s) == nil || s.aiInputPaused {
		m.finishRecoveryLocked(s)
		s.mu.Unlock()
		return
	}
	// ICE만 다시 연결되고 카메라가 멈춘 경우 취소 슬레이트를 너무 일찍 해제하지
	// 않는다. processImages가 첫 정상 처리 프레임을 확인한 뒤 완료시킨다.
	s.recovery.awaitingMediaFrame = true
	s.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()
}

// noteProcessedMediaFrame은 AI 처리까지 끝난 프레임만 복구 성공으로 인정한다.
// 단순 RTP 수신이나 ICE connected 상태만으로는 RTMP 시청자가 카메라 화면을 다시
// 보게 됐다는 보장이 없기 때문이다.
func (m *Manager) noteProcessedMediaFrame(s *Session) {
	s.mu.Lock()
	if s.closed || s.recovery.status == PeerRecoveryStatusIdle || !s.recovery.awaitingMediaFrame || s.PC.ConnectionState().String() != "connected" {
		s.mu.Unlock()
		return
	}
	m.finishRecoveryLocked(s)
	s.mu.Unlock()
}

func (m *Manager) finishRecoveryLocked(s *Session) {
	if s.recovery.status == PeerRecoveryStatusIdle {
		return
	}
	started := s.recovery.deadline != nil
	attempts := s.recovery.attempts
	if s.recovery.debounceTimer != nil {
		s.recovery.debounceTimer.Stop()
		s.recovery.debounceTimer = nil
	}
	if s.recovery.deadlineTimer != nil {
		s.recovery.deadlineTimer.Stop()
		s.recovery.deadlineTimer = nil
	}
	if egress := activeEgressLocked(s); egress != nil {
		egress.EndInputRecovery()
	}
	s.recovery.status = PeerRecoveryStatusIdle
	s.recovery.deadline = nil
	s.recovery.lastError = nil
	s.recovery.awaitingMediaFrame = false
	s.recovery.attempts = 0
	s.UpdatedAt = time.Now().UTC()
	if started {
		m.metrics.IncReconnects()
		m.metrics.IncPeerRecoverySucceeded()
		m.logger.Info("WebRTC network recovery succeeded", "session_id", s.ID, "reconnect_attempts", attempts)
	}
}

func (s *Session) cancelRecoveryLocked() {
	if s.recovery.debounceTimer != nil {
		s.recovery.debounceTimer.Stop()
		s.recovery.debounceTimer = nil
	}
	if s.recovery.deadlineTimer != nil {
		s.recovery.deadlineTimer.Stop()
		s.recovery.deadlineTimer = nil
	}
	s.recovery.status = PeerRecoveryStatusIdle
	s.recovery.deadline = nil
	s.recovery.lastError = nil
	s.recovery.awaitingMediaFrame = false
	s.recovery.attempts = 0
	s.recovery.generation++
}

func activeEgressLocked(s *Session) *media.RTMPEgress {
	if s.egress == nil || s.streamStopReason != nil || s.egress.Status().Phase == media.EgressPhaseStopped {
		return nil
	}
	return s.egress
}
