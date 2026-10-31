package session

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/media"
	"inno-live-server/internal/plan"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
)

// 서버 교체 배포(#517): 옛 프로세스가 세션 상태를 스냅샷으로 새 프로세스에 넘기고,
// 클라이언트는 연결이 끊긴 뒤 같은 세션 id로 새 프로세스와 다시 협상한다. 새 프로세스는
// 세션을 재연결 대기(복구 중)로 받아 두고, 클라이언트가 다시 붙으면 송출을 이어 간다.
// WebRTC·FFmpeg·AI 스트림 같은 프로세스 자원은 넘기지 않고 새로 만든다.

var (
	// ErrHandoffBusy는 플랫폼 왕복(방송 준비·라이브 전환)이나 해상도 변경 중이라 지금
	// 넘기면 그 결과를 잃는 세션이다. 잠시 뒤 다시 시도한다.
	ErrHandoffBusy = errors.New("session is in a platform transition; retry the handoff")
	// ErrHandoffUnsupported는 넘기지 않는 세션이다. 게스트 세션은 대기열 상태가
	// 프로세스 밖에 있어 지금은 넘기지 않는다.
	ErrHandoffUnsupported = errors.New("session cannot be handed off")
	// ErrHandoffConflict는 새 프로세스에 이미 같은 id의 세션이 클라이언트와 연결돼 있을
	// 때다. 이관이 끝난 세션을 덮어쓰지 않는다.
	ErrHandoffConflict = errors.New("session is already live on this server")
)

// SessionSnapshot은 다른 프로세스로 넘기는 세션 상태다. 송출 URL(스트림 키)과 소유자
// 토큰 해시가 들어 있으므로 루프백 제어 API로만 주고받는다.
type SessionSnapshot struct {
	ID                   string                    `json:"id"`
	UserID               uuid.UUID                 `json:"user_id"`
	Provider             string                    `json:"provider"`
	Plan                 plan.Plan                 `json:"plan"`
	AIProcessing         string                    `json:"ai_processing"`
	FaceAnonymization    string                    `json:"face_anonymization"`
	BroadcastResolution  string                    `json:"broadcast_resolution"`
	AIClientID           string                    `json:"ai_client_id"`
	CreatedAt            time.Time                 `json:"created_at"`
	Metadata             map[string]string         `json:"metadata,omitempty"`
	OwnerHash            string                    `json:"owner_hash"`
	Timing               Timing                    `json:"timing"`
	AnonymizationEnabled bool                      `json:"anonymization_enabled"`
	ScreenBlurEnabled    bool                      `json:"screen_blur_enabled"`
	MosaicConfig         media.MosaicConfig        `json:"mosaic_config"`
	Broadcast            *YouTubeBroadcastSettings `json:"broadcast,omitempty"`
	ChzzkBroadcast       *ChzzkBroadcastSettings   `json:"chzzk_broadcast,omitempty"`
	Notices              []Notice                  `json:"notices,omitempty"`
	BroadcastRemaining   *int64                    `json:"broadcast_remaining,omitempty"`
	DirectStops          map[string]string         `json:"direct_stops,omitempty"`
	Targets              []TargetSnapshot          `json:"targets"`
}

// TargetSnapshot은 송출 대상 하나의 상태다.
type TargetSnapshot struct {
	Provider          string             `json:"provider"`
	Phase             BroadcastPhase     `json:"phase"`
	PlatformBroadcast *PlatformBroadcast `json:"platform_broadcast,omitempty"`
	Channel           string             `json:"channel,omitempty"`
	// UsageID는 앱이 직접 송출하는 대상(egress 없음)의 송출 세대다. 서버 egress
	// 대상은 옛 프로세스가 세대를 닫고 새 프로세스가 새 세대를 연다.
	UsageID string `json:"usage_id,omitempty"`
	// Egress는 서버가 RTMP로 송출하던 대상이다. 새 프로세스가 같은 URL로 다시 붙는다.
	Egress *EgressSnapshot `json:"egress,omitempty"`
}

// EgressSnapshot은 이어 갈 RTMP 송출이다.
type EgressSnapshot struct {
	OutputURL           string        `json:"output_url"`
	Paused              bool          `json:"paused"`
	ReconnectMaxElapsed time.Duration `json:"reconnect_max_elapsed"`
}

// sessionHandoff는 넘겨받은 세션의 이어 가기 상태다. Session.mu로 보호한다.
type sessionHandoff struct {
	// committed는 옛 프로세스가 세션을 놓았다는 표시다. 그 전에는 옛 프로세스가 아직
	// 같은 스트림 키로 송출 중일 수 있으므로 송출을 다시 붙이지 않는다.
	committed bool
	// egress는 아직 다시 붙이지 않은 송출 대상이다.
	egress map[string]EgressSnapshot
}

// ExportSession은 세션을 다른 프로세스로 넘길 스냅샷을 만든다. 세션은 그대로 둔다.
func (m *Manager) ExportSession(id string) (SessionSnapshot, error) {
	s, err := m.Get(id)
	if err != nil {
		return SessionSnapshot{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return SessionSnapshot{}, ErrNotFound
	}
	if s.GuestID != "" {
		return SessionSnapshot{}, fmt.Errorf("%w: guest session", ErrHandoffUnsupported)
	}
	if s.resolutionSwitch != nil && s.resolutionSwitch.Status == "switching" {
		return SessionSnapshot{}, ErrHandoffBusy
	}
	snapshot := SessionSnapshot{
		ID:                   s.ID,
		UserID:               s.UserID,
		Provider:             s.Provider,
		Plan:                 s.Plan,
		AIProcessing:         s.AIProcessing,
		FaceAnonymization:    s.FaceAnonymization,
		BroadcastResolution:  s.BroadcastResolution,
		AIClientID:           s.AIClientID,
		CreatedAt:            s.CreatedAt,
		Metadata:             copyMetadata(s.Metadata),
		OwnerHash:            hex.EncodeToString(s.ownerHash[:]),
		Timing:               s.Timing,
		AnonymizationEnabled: s.anonymizationEnabled,
		ScreenBlurEnabled:    s.screenBlurEnabled,
		MosaicConfig:         s.mosaicConfig.Clone(),
		Notices:              append([]Notice(nil), s.notices...),
	}
	if s.broadcast != nil {
		copy := *s.broadcast
		snapshot.Broadcast = &copy
	}
	if s.chzzkBroadcast != nil {
		copy := *s.chzzkBroadcast
		snapshot.ChzzkBroadcast = &copy
	}
	if s.broadcastRemaining != nil {
		remaining := *s.broadcastRemaining
		snapshot.BroadcastRemaining = &remaining
	}
	if len(s.directStops) > 0 {
		snapshot.DirectStops = copyMetadata(s.directStops)
	}
	for provider, t := range s.targets {
		if t.phase == BroadcastPhasePreparing || t.phase == BroadcastPhaseGoingLive {
			return SessionSnapshot{}, ErrHandoffBusy
		}
		target := TargetSnapshot{Provider: provider, Phase: t.phase, Channel: t.channel}
		if t.platformBroadcast != nil {
			broadcast := *t.platformBroadcast
			target.PlatformBroadcast = &broadcast
		}
		if t.egress != nil && t.stopReason == nil {
			status := t.egress.Status()
			if status.Phase != media.EgressPhaseStopped {
				target.Egress = &EgressSnapshot{
					OutputURL: t.egress.OutputURL(),
					// 재연결 중인 일시 중지도 일시 중지다. 빠뜨리면 이관 뒤 송출이 풀린다.
					Paused: status.Phase == media.EgressPhasePaused || status.Phase == media.EgressPhasePausedReconfiguring ||
						status.Phase == media.EgressPhasePausedReconnecting,
					ReconnectMaxElapsed: t.egress.ReconnectMaxElapsed(),
				}
			}
		} else if t.egress == nil {
			target.UsageID = t.usageID
		}
		snapshot.Targets = append(snapshot.Targets, target)
	}
	return snapshot, nil
}

// ImportSession은 다른 프로세스가 넘긴 세션을 재연결 대기 상태로 등록한다. 새
// PeerConnection을 만들어 두고, 클라이언트가 같은 세션 id로 보내는 offer(일반 offer든
// ICE restart offer든)를 첫 협상으로 받는다. 복구 창 안에 다시 붙지 않으면 평소의
// 복구 소진과 같이 세션을 정리한다.
//
// commit은 옛 프로세스가 세션을 이미 놓았는지다. 놓기 전에 받아 두는 준비 단계에서는
// false로 부르고, 놓은 뒤 같은 스냅샷(최신)을 commit=true로 다시 보낸다. 아직
// 클라이언트가 붙지 않은 세션은 최신 스냅샷으로 바꾼다.
func (m *Manager) ImportSession(snapshot SessionSnapshot, commit bool) error {
	ownerHash, err := decodeOwnerHash(snapshot.OwnerHash)
	if err != nil {
		return err
	}
	if _, err := uuid.Parse(snapshot.ID); err != nil {
		return fmt.Errorf("invalid session id: %w", err)
	}
	if len(snapshot.Targets) == 0 {
		return errors.New("session snapshot has no targets")
	}

	m.mu.Lock()
	existing := m.sessions[snapshot.ID]
	m.mu.Unlock()
	if existing != nil {
		existing.mu.Lock()
		placeholder := existing.handoff != nil && !existing.wasConnected && existing.answerCreatedAt.IsZero()
		if !placeholder {
			existing.mu.Unlock()
			if commit && existing.handoff != nil {
				// 클라이언트가 옛 프로세스가 놓기 전에 먼저 넘어왔다. 상태는 이미 이쪽이
				// 최신이므로 놓았다는 사실만 반영한다.
				m.commitHandoff(existing)
				return nil
			}
			return ErrHandoffConflict
		}
		existing.mu.Unlock()
		m.discardPlaceholder(existing)
	}

	var serverICE []webrtc.ICEServer
	if m.cfg.AnnouncedIP == "" {
		serverICE = m.ice
	}
	pc, err := m.api.NewPeerConnection(webrtc.Configuration{ICEServers: serverICE})
	if err != nil {
		return fmt.Errorf("create PeerConnection: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC()
	privacyMode := m.cfg.PrivacyMode
	if snapshot.AIProcessing == AIProcessingOnDevice {
		privacyMode = config.PrivacyModeBypass
	}
	s := &Session{
		AIProcessing:         snapshot.AIProcessing,
		FaceAnonymization:    snapshot.FaceAnonymization,
		privacyMode:          privacyMode,
		ID:                   snapshot.ID,
		UserID:               snapshot.UserID,
		Provider:             snapshot.Provider,
		Plan:                 snapshot.Plan,
		BroadcastResolution:  snapshot.BroadcastResolution,
		pinLongEdge:          pinLongEdgeFor(snapshot.BroadcastResolution, m.cfg.DecoderPinLongEdge),
		AIClientID:           snapshot.AIClientID,
		CreatedAt:            snapshot.CreatedAt,
		UpdatedAt:            now,
		lastActivityAt:       now,
		Metadata:             copyMetadata(snapshot.Metadata),
		Status:               "active",
		PC:                   pc,
		Timing:               snapshot.Timing,
		Stream:               StreamState{Status: "idle", UpdatedAt: now, BroadcastPhase: BroadcastPhaseIdle},
		anonymizationEnabled: snapshot.AnonymizationEnabled && privacyMode == config.PrivacyModeReal,
		screenBlurEnabled:    snapshot.ScreenBlurEnabled,
		mosaicConfig:         snapshot.MosaicConfig.Clone(),
		broadcast:            snapshot.Broadcast,
		chzzkBroadcast:       snapshot.ChzzkBroadcast,
		notices:              append([]Notice(nil), snapshot.Notices...),
		broadcastRemaining:   snapshot.BroadcastRemaining,
		ownerHash:            ownerHash,
		cancel:               cancel,
		// 클라이언트는 연결이 끊긴 뒤 다시 붙는다. 이전 프로세스에서 연결돼 있었으므로
		// 복구 중으로 시작해 ICE restart offer를 복구 시도로 받아들이고, 복구 창이
		// 끝나면 평소처럼 정리한다.
		wasConnected: false,
		recovery:     peerRecovery{status: PeerRecoveryStatusRecovering, generation: 1, lastError: recoveryError("server_handoff")},
		handoff:      &sessionHandoff{committed: commit, egress: make(map[string]EgressSnapshot)},
	}
	if len(snapshot.DirectStops) > 0 {
		s.directStops = copyMetadata(snapshot.DirectStops)
	}
	s.baseCtx = ctx
	s.egressFanout = media.NewEgressFanout()
	s.mosaic = media.NewMosaicTally()
	s.targets = make(map[string]*streamTarget, len(snapshot.Targets))
	for _, target := range snapshot.Targets {
		t := &streamTarget{phase: target.Phase, channel: target.Channel, usageID: target.UsageID}
		if t.phase == "" {
			t.phase = BroadcastPhaseIdle
		}
		if target.PlatformBroadcast != nil {
			broadcast := *target.PlatformBroadcast
			t.platformBroadcast = &broadcast
		}
		if target.Egress != nil && target.Egress.OutputURL != "" {
			s.handoff.egress[target.Provider] = *target.Egress
		}
		s.targets[target.Provider] = t
	}
	if _, ok := s.targets[s.Provider]; !ok {
		s.targets[s.Provider] = &streamTarget{phase: BroadcastPhaseIdle}
	}
	if m.cfg.EnableAudioEgress {
		s.audioPipe = media.NewAudioPipe(m.logger.With("session_id", s.ID), m.metrics, 2)
		go s.audioPipe.Run(ctx)
	}
	m.installHandlers(ctx, s)

	deadline := now.Add(recoveryWindow(m.cfg))
	s.recovery.deadline = &deadline
	s.recovery.deadlineTimer = time.AfterFunc(time.Until(deadline), func() {
		m.expireRecovery(s.ID, 1)
	})

	m.mu.Lock()
	if current := m.sessions[s.ID]; current != nil {
		m.mu.Unlock()
		s.close("server_handoff_conflict", m.logger)
		return ErrHandoffConflict
	}
	m.sessions[s.ID] = s
	count := len(m.sessions)
	m.mu.Unlock()
	m.metrics.SetActiveSessions(count)
	m.logger.Info("imported handed-off session", "session_id", s.ID, "user_id", s.UserID,
		"committed", commit, "egress_targets", len(s.handoff.egress))
	return nil
}

// CommitHandoff는 옛 프로세스가 세션을 놓았다고 표시하고, 클라이언트가 이미 다시
// 붙었으면 송출을 다시 붙인다.
func (m *Manager) CommitHandoff(id string) error {
	s, err := m.Get(id)
	if err != nil {
		return err
	}
	m.commitHandoff(s)
	return nil
}

func (m *Manager) commitHandoff(s *Session) {
	s.mu.Lock()
	if s.handoff == nil {
		s.mu.Unlock()
		return
	}
	s.handoff.committed = true
	s.mu.Unlock()
	m.resumeHandoffEgress(s)
}

// resumeHandoffEgress는 넘겨받은 송출 대상을 같은 URL로 다시 붙인다. 옛 프로세스가
// 세션을 놓았고(committed) 클라이언트 영상이 다시 들어온 뒤에만 한다. 그 전에는
// 붙일 영상이 없거나 같은 스트림 키에 두 연결이 겹친다.
func (m *Manager) resumeHandoffEgress(s *Session) {
	s.mu.Lock()
	if s.closed || s.handoff == nil || !s.handoff.committed || s.rawTrackID == "" || len(s.handoff.egress) == 0 {
		s.mu.Unlock()
		return
	}
	pending := s.handoff.egress
	s.handoff.egress = make(map[string]EgressSnapshot)
	s.mu.Unlock()

	for provider, egress := range pending {
		// 일시 중지는 시작할 때 싣는다. 시작 뒤에 멈추면 첫 프로필이 정해질 때까지
		// pause가 거절돼(idle) 카메라와 마이크가 그대로 나간다.
		options := StreamOptions{Provider: provider, ReconnectMaxElapsed: egress.ReconnectMaxElapsed, Paused: egress.Paused}
		if _, err := m.StartStream(s.ID, egress.OutputURL, options); err != nil {
			m.logger.Error("resuming handed-off RTMP egress failed", "session_id", s.ID, "provider", provider, "error", err)
			continue
		}
		m.logger.Info("resumed handed-off RTMP egress", "session_id", s.ID, "provider", provider, "paused", egress.Paused)
	}
}

// handoffPending은 옛 프로세스가 아직 놓지 않은 넘겨받은 세션인지다. 이 세션의
// 플랫폼 방송·실사용 기록·정리 훅은 아직 옛 프로세스의 것이다. 배포가 롤백되거나
// commit이 실패해 이 세션을 버릴 때 정리하면 옛 프로세스에서 송출 중인 방송이 끝난다.
func (s *Session) handoffPending() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.handoff != nil && !s.handoff.committed
}

// DetachSession은 다른 프로세스로 넘긴 세션을 이 프로세스에서 놓는다. 평소 삭제와
// 달리 플랫폼 방송을 끝내지 않고, 세션 종료 실사용 기록·정리 훅도 부르지 않는다 —
// 세션은 새 프로세스에서 이어진다. 서버 egress는 끊고(그 세대의 실사용은
// server_handoff로 닫힌다) PeerConnection을 닫아 클라이언트가 다시 붙게 한다.
func (m *Manager) DetachSession(id string) error {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		m.mu.Unlock()
		return ErrNotFound
	}
	delete(m.sessions, id)
	count := len(m.sessions)
	m.mu.Unlock()
	m.egressSlots.ReleaseHold(id)
	m.metrics.SetActiveSessions(count)

	s.mu.Lock()
	reason := "server_handoff"
	for _, t := range s.targets {
		// runEgress가 egress 종료 뒤 플랫폼 방송을 치우지 않도록 떼어 둔다.
		t.platformBroadcast = nil
		t.phase = BroadcastPhaseIdle
		if t.egress != nil && t.stopReason == nil {
			t.stopReason = &reason
		}
	}
	s.mu.Unlock()
	s.close(reason, m.logger)
	m.logger.Info("detached handed-off session", "session_id", id)
	return nil
}

// discardPlaceholder는 아직 클라이언트가 붙지 않은 넘겨받은 세션을 정리 훅 없이
// 버린다. 같은 세션의 최신 스냅샷으로 바꿀 때 쓴다.
func (m *Manager) discardPlaceholder(s *Session) {
	m.mu.Lock()
	if m.sessions[s.ID] == s {
		delete(m.sessions, s.ID)
	}
	m.mu.Unlock()
	s.mu.Lock()
	s.cancelRecoveryLocked()
	s.mu.Unlock()
	s.close("server_handoff_replaced", m.logger)
}

func decodeOwnerHash(value string) ([32]byte, error) {
	var hash [32]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(hash) {
		return hash, errors.New("invalid owner hash")
	}
	copy(hash[:], decoded)
	return hash, nil
}
