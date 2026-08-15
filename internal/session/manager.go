package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"inno-live-server/internal/ai"
	"inno-live-server/internal/config"
	"inno-live-server/internal/media"
	"inno-live-server/internal/metrics"

	"github.com/google/uuid"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

var (
	ErrNotFound         = errors.New("session not found")
	ErrCapacityExceeded = errors.New("session capacity exceeded")
	ErrNoVideoTrack     = errors.New("no video track is available")
	ErrStreamActive     = errors.New("stream egress is already active")
	ErrStreamNotActive  = errors.New("stream egress is not active")
	ErrStreamPaused     = errors.New("stream egress is already paused")
	ErrStreamNotPaused  = errors.New("stream egress is not paused")
)

type Timing struct {
	SessionToOfferMS       *int64 `json:"session_to_offer_ms"`
	OfferToAnswerMS        *int64 `json:"offer_to_answer_ms"`
	OfferToConnectedMS     *int64 `json:"offer_to_connected_ms"`
	AnswerToConnectedMS    *int64 `json:"answer_to_connected_ms"`
	OfferToICECompletedMS  *int64 `json:"offer_to_ice_completed_ms"`
	AnswerToICECompletedMS *int64 `json:"answer_to_ice_completed_ms"`
}

type StreamState struct {
	Status            string     `json:"status"`
	StartedAt         *time.Time `json:"started_at"`
	StoppedAt         *time.Time `json:"stopped_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	TargetURL         *string    `json:"target_url"`
	PublisherActive   bool       `json:"publisher_active"`
	LastError         *string    `json:"last_error"`
	StopReason        *string    `json:"stop_reason"`
	ReconnectAttempts int        `json:"reconnect_attempts"`
	VideoWidth        uint16     `json:"video_width"`
	VideoHeight       uint16     `json:"video_height"`
	VideoFPS          int        `json:"video_fps"`
	PausedAt          *time.Time `json:"paused_at"`
}

type TrackState struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	ReadyState string `json:"ready_state"`
}

type Response struct {
	SessionID string            `json:"session_id"`
	Status    string            `json:"status"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Metadata  map[string]string `json:"metadata"`
	Peer      struct {
		ConnectionState    string `json:"connection_state"`
		ICEConnectionState string `json:"ice_connection_state"`
		SignalingState     string `json:"signaling_state"`
	} `json:"peer_connection"`
	Timing Timing `json:"timing"`
	Media  struct {
		RawVideoTrack          *TrackState `json:"raw_video_track"`
		ProcessedVideoTrack    *TrackState `json:"processed_video_track"`
		VideoSenderActive      bool        `json:"video_sender_active"`
		IgnoredTrackCount      int         `json:"ignored_track_count"`
		UnsupportedTrackPolicy string      `json:"unsupported_track_policy"`
		AIFallbackActive       bool        `json:"ai_fallback_active"`
	} `json:"media"`
	Stream StreamState `json:"stream"`
}

type Session struct {
	mu sync.RWMutex

	ID         string
	UserID     uuid.UUID
	AIClientID string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Metadata   map[string]string
	Status     string
	PC         *webrtc.PeerConnection
	Output     *webrtc.TrackLocalStaticSample
	VideoCodec media.VideoCodec
	Sender     *webrtc.RTPSender
	Timing     Timing
	Stream     StreamState

	ownerHash        [32]byte
	rawTrackID       string
	processedTrackID string
	audioTrackID     string
	audioPipe        *media.AudioPipe
	// egress 수명은 명시적 start~stop 구간이다(#83). 트랙 수명이 아니라(#84)
	// 카메라 전환에도 살아남고, 파이프라인은 egressSlot을 통해서만 참조하므로
	// 트랙 도착 이후에 시작해도 실행 중인 파이프라인에 꽂힌다.
	// baseCtx는 세션 수명 컨텍스트로, start 시점의 egress 컨텍스트 파생원이다.
	baseCtx          context.Context
	egressSlot       *media.EgressSlot
	egress           *media.RTMPEgress
	egressCancel     context.CancelFunc
	streamStopReason *string
	processor        *media.Processor
	ignoredTracks    int
	offerReceivedAt  time.Time
	answerCreatedAt  time.Time
	pendingICE       []webrtc.ICECandidateInit
	negotiationMu    sync.Mutex
	cancel           context.CancelFunc
	trackCancel      context.CancelFunc
	disconnectTimer  *time.Timer
	wasConnected     bool
	closed           bool
}

type Manager struct {
	cfg       config.Config
	logger    *slog.Logger
	metrics   *metrics.Registry
	ai        *ai.Pool
	spawnGate *media.SpawnGate
	api       *webrtc.API
	ice       []webrtc.ICEServer

	mu       sync.RWMutex
	sessions map[string]*Session
	pending  int
	// pipelines tracks sessions whose media pipeline (FFmpeg pair) is still
	// alive, including sessions already removed from the map but not yet
	// torn down — those keep counting toward MaxSessions so reconnect churn
	// cannot double-book processes during the teardown grace window.
	pipelines map[string]struct{}
}

// streamPauseController는 일시 중지·재개 제어에 필요한 egress 동작만
// 나타낸다. 상태 확인과 제어 호출 사이의 종료 경합을 별도 테스트할 수 있게
// 최소 계약으로 둔다.
type streamPauseController interface {
	Status() media.EgressStatus
	Pause() bool
	Resume() bool
}

func NewManager(cfg config.Config, logger *slog.Logger, registry *metrics.Registry, aiPool *ai.Pool, spawnGate *media.SpawnGate) (*Manager, error) {
	if cfg.PrivacyMode == config.PrivacyModeReal && aiPool == nil {
		return nil, errors.New("real privacy mode requires an AI client pool")
	}
	var settingEngine webrtc.SettingEngine
	if cfg.UDPMuxPort > 0 {
		// Multiplex all ICE UDP traffic onto a single port. Spreading media
		// across the ephemeral range (the default) opens one NAT binding per
		// flow, which overwhelms home-router NAT tables under multi-session
		// load — observed as heavy RTP packet loss once the client runs on a
		// separate machine. A single muxed port collapses that to one binding
		// (and means only one UDP port needs opening on a firewall).
		udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: cfg.UDPMuxPort})
		if err != nil {
			return nil, fmt.Errorf("open WebRTC UDP mux port %d: %w", cfg.UDPMuxPort, err)
		}
		settingEngine.SetICEUDPMux(webrtc.NewICEUDPMux(logging.NewDefaultLoggerFactory().NewLogger("ice-udp-mux"), udpConn))
	} else if err := settingEngine.SetEphemeralUDPPortRange(cfg.UDPPortMin, cfg.UDPPortMax); err != nil {
		return nil, fmt.Errorf("configure WebRTC UDP port range: %w", err)
	}
	if cfg.AnnouncedIP != "" {
		settingEngine.SetNAT1To1IPs([]string{cfg.AnnouncedIP}, webrtc.ICECandidateTypeHost)
	}
	iceServers := buildICEServers(cfg)
	return &Manager{
		cfg:       cfg,
		logger:    logger,
		metrics:   registry,
		ai:        aiPool,
		spawnGate: spawnGate,
		api:       webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine)),
		ice:       iceServers,
		sessions:  make(map[string]*Session),
		pipelines: make(map[string]struct{}),
	}, nil
}

func (m *Manager) ICEServers() []webrtc.ICEServer {
	result := make([]webrtc.ICEServer, len(m.ice))
	copy(result, m.ice)
	return result
}

// Capacity reports the number of capacity-consuming sessions (live +
// reserved) and the configured limit (0 means unlimited).
func (m *Manager) Capacity() (active, limit int) {
	m.mu.RLock()
	active = len(m.sessions) + m.pending
	m.mu.RUnlock()
	return active, m.cfg.MaxSessions
}

// capacityInUseLocked counts live sessions, in-flight reservations, and
// orphaned pipelines (deleted sessions whose FFmpeg pair is still tearing
// down). Callers must hold m.mu.
func (m *Manager) capacityInUseLocked() int {
	inUse := len(m.sessions) + m.pending
	for id := range m.pipelines {
		if _, live := m.sessions[id]; !live {
			inUse++
		}
	}
	return inUse
}

// Create provisions a new session and returns it together with the plaintext
// owner token. The token is returned exactly once here; the server must relay
// it to the creator in the creation response and never expose it again.
func (m *Manager) Create(metadata map[string]string) (*Session, string, error) {
	return m.CreateForUser(uuid.Nil, metadata)
}

// CreateForUser provisions a session owned by userID. The AI client ID is
// deliberately derived here rather than accepted as request metadata so the
// face whitelist and the media stream always use the same authenticated scope.
func (m *Manager) CreateForUser(userID uuid.UUID, metadata map[string]string) (*Session, string, error) {
	if limit := m.cfg.MaxSessions; limit > 0 {
		m.mu.Lock()
		if m.capacityInUseLocked() >= limit {
			active := len(m.sessions)
			m.mu.Unlock()
			m.metrics.IncSessionsRejected()
			return nil, "", fmt.Errorf("%w: active=%d limit=%d", ErrCapacityExceeded, active, limit)
		}
		m.pending++
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			m.pending--
			m.mu.Unlock()
		}()
	}
	ownerToken, ownerHash, err := newOwnerToken()
	if err != nil {
		return nil, "", fmt.Errorf("generate owner token: %w", err)
	}
	pc, err := m.api.NewPeerConnection(webrtc.Configuration{ICEServers: m.ice})
	if err != nil {
		return nil, "", fmt.Errorf("create PeerConnection: %w", err)
	}
	id := uuid.NewString()
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC()
	s := &Session{
		ID:         id,
		UserID:     userID,
		AIClientID: AIClientIDForUser(userID),
		CreatedAt:  now,
		UpdatedAt:  now,
		Metadata:   copyMetadata(metadata),
		Status:     "active",
		PC:         pc,
		Stream:     StreamState{Status: "idle", UpdatedAt: now},
		ownerHash:  ownerHash,
		cancel:     cancel,
	}
	s.baseCtx = ctx
	s.egressSlot = media.NewEgressSlot()
	// The audio pipe is created up front (decoupled from track arrival order)
	// so the egress can attach it regardless of whether the video or the audio
	// track's OnTrack fires first. It idles, dropping samples, until a mic track
	// feeds it and the egress attaches a pipe. 사용자별 송출(#83)에서는 세션
	// 생성 시점에 송출 여부를 알 수 없으므로 오디오 egress 설정만 보고 만든다.
	if m.cfg.EnableAudioEgress {
		s.audioPipe = media.NewAudioPipe(m.logger.With("session_id", id), m.metrics, 2)
		go s.audioPipe.Run(ctx)
	}
	m.installHandlers(ctx, s)
	m.mu.Lock()
	m.sessions[id] = s
	count := len(m.sessions)
	m.mu.Unlock()
	m.metrics.SetActiveSessions(count)
	m.metrics.IncConnections()
	if timeout := m.cfg.NegotiationTimeout; timeout > 0 {
		time.AfterFunc(timeout, func() { m.reapUnnegotiated(id) })
	}
	m.logger.Info("created live session", "session_id", id, "user_id", userID)
	return s, ownerToken, nil
}

// AIClientIDForUser is the only whitelist bucket identifier used for an
// authenticated user. The prefix keeps it distinct from the legacy global
// bucket (an empty client ID) without exposing a caller-controlled selector.
func AIClientIDForUser(userID uuid.UUID) string {
	return "user:" + userID.String()
}

// reapUnnegotiated frees a session that never started negotiation within
// SESSION_NEGOTIATION_TIMEOUT, so bare POST /sessions calls cannot pin
// MAX_SESSIONS slots indefinitely.
func (m *Manager) reapUnnegotiated(id string) {
	s, err := m.Get(id)
	if err != nil {
		return
	}
	s.mu.RLock()
	bare := s.offerReceivedAt.IsZero() && !s.wasConnected && !s.closed
	s.mu.RUnlock()
	if !bare {
		return
	}
	m.logger.Info("reaping unnegotiated session", "session_id", id, "timeout", m.cfg.NegotiationTimeout)
	_ = m.Delete(id, "negotiation_timeout")
}

func (m *Manager) Get(id string) (*Session, error) {
	m.mu.RLock()
	s := m.sessions[id]
	m.mu.RUnlock()
	if s == nil {
		return nil, ErrNotFound
	}
	return s, nil
}

// VerifyOwner resolves a session and confirms the caller owns it. It returns
// ErrNotFound when no such session exists and ErrUnauthorized when the token
// does not match. When session auth is disabled (local dev only) the token is
// not checked, but the session must still exist. This is the single gate used
// by both the HTTP middleware and the WebRTC signaling path.
func (m *Manager) VerifyOwner(id, token string) (*Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	if m.cfg.RequireSessionAuth && !s.verifyOwnerToken(token) {
		return nil, ErrUnauthorized
	}
	return s, nil
}

func (m *Manager) List() []*Session {
	m.mu.RLock()
	result := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		result = append(result, s)
	}
	m.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result
}

// CloseUserSessions는 탈퇴한 사용자의 모든 활성 세션을 종료한다. 세션 소유권은
// 메모리에 있으므로, 이후 API 요청을 거부하는 DB 계정 상태 확인과 함께 쓴다.
func (m *Manager) CloseUserSessions(userID uuid.UUID) {
	m.closeUserSessions(userID, "user_withdrawal")
}

// CloseUserSessionsForLogout는 로그아웃한 사용자의 모든 활성 세션을 즉시
// 종료한다. Session.close가 egress context를 취소하면 RTMPEgress.Run은 stop
// 이벤트를 거쳐 stopped로 전이한다.
func (m *Manager) CloseUserSessionsForLogout(userID uuid.UUID) {
	m.closeUserSessions(userID, "user_logout")
}

func (m *Manager) closeUserSessions(userID uuid.UUID, reason string) {
	m.mu.RLock()
	ids := make([]string, 0)
	for id, liveSession := range m.sessions {
		if liveSession.UserID == userID {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range ids {
		if err := m.Delete(id, reason); err != nil && !errors.Is(err, ErrNotFound) {
			m.logger.Warn("close user session failed", "session_id", id, "user_id", userID, "reason", reason, "error", err)
		}
	}
}

// StartStream은 세션의 처리(블러) 완료 출력에 RTMP egress를 붙인다.
// outputURL은 스트림 키가 포함된 완성 URL이므로 로그에 남기지 않는다
// (egress가 자체 마스킹으로 기록한다). 파이프라인이 이미 돌고 있어도
// egressSlot을 통해 즉시 프레임이 흐르기 시작한다.
func (m *Manager) StartStream(id, outputURL string) (*Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrNotFound
	}
	// 트랙 먼저, 시작 나중 — 종전 501 스텁 시절부터의 계약(409)을 유지한다.
	if s.rawTrackID == "" {
		return nil, ErrNoVideoTrack
	}
	// 중지가 요청된(streamStopReason != nil) egress는 종료 절차 중이므로
	// 활성으로 보지 않는다 — stop 직후의 재시작이 이전 Run 고루틴의 종료
	// 타이밍에 좌우되면 안 된다.
	if s.egress != nil && s.streamStopReason == nil && s.egress.Status().Phase != media.EgressPhaseStopped {
		return nil, ErrStreamActive
	}
	egressCtx, egressCancel := context.WithCancel(s.baseCtx)
	egress := media.NewRTMPEgress(m.cfg.FFmpegPath, m.logger.With("session_id", s.ID), m.metrics, media.TranscoderOptions{
		Gate:       m.spawnGate,
		WireFormat: m.cfg.AIWireFormat,
	}, outputURL, s.audioPipe, m.cfg.EgressLatencyLog, m.cfg.EgressAudioOffset, m.cfg.EgressVideoBitrate)
	s.egress = egress
	s.egressCancel = egressCancel
	s.streamStopReason = nil
	s.egressSlot.Set(egress)
	s.UpdatedAt = time.Now().UTC()
	go egress.Run(egressCtx)
	m.logger.Info("RTMP egress started", "session_id", s.ID, "url", egress.Status().TargetURL)
	return s, nil
}

// StopStream은 egress만 종료한다 — 세션과 뷰어(WebRTC) 송출은 유지된다.
// 플랫폼 쪽 방송 종료는 enableAutoStop이 담당한다(송출 중단 약 1분 후 반영,
// 실측 57.6초).
func (m *Manager) StopStream(id string) (*Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.egress == nil || s.streamStopReason != nil || s.egress.Status().Phase == media.EgressPhaseStopped {
		return nil, ErrStreamNotActive
	}
	s.egressSlot.Clear()
	s.egress.Stop()
	s.egressCancel()
	reason := "user_requested"
	s.streamStopReason = &reason
	s.UpdatedAt = time.Now().UTC()
	m.logger.Info("RTMP egress stopped", "session_id", s.ID, "reason", reason)
	return s, nil
}

// PauseStream은 RTMP 연결을 유지한 채 실행 중인 egress를 일시 중단 상태로
// 표시한다. 미디어 계층은 기록된 출력 형식으로 취소 슬레이트를 공급하게 되며,
// 이 제어 동작 자체는 플랫폼 방송을 종료하지 않는다.
func (m *Manager) PauseStream(id string) (*Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.egress == nil || s.streamStopReason != nil {
		return nil, ErrStreamNotActive
	}
	if err := pauseEgress(s.egress); err != nil {
		return nil, err
	}
	s.UpdatedAt = time.Now().UTC()
	m.logger.Info("RTMP egress paused", "session_id", s.ID)
	return s, nil
}

// ResumeStream은 일시 중단된 egress를 다시 실제 영상 상태로 표시한다. 같은
// egress를 계속 사용하므로 플랫폼 방송과 측정된 출력 형식도 유지된다.
func (m *Manager) ResumeStream(id string) (*Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.egress == nil || s.streamStopReason != nil {
		return nil, ErrStreamNotActive
	}
	if err := resumeEgress(s.egress); err != nil {
		return nil, err
	}
	s.UpdatedAt = time.Now().UTC()
	m.logger.Info("RTMP egress resumed", "session_id", s.ID)
	return s, nil
}

func pauseEgress(egress streamPauseController) error {
	switch egress.Status().Phase {
	case media.EgressPhaseStopped:
		return ErrStreamNotActive
	case media.EgressPhasePaused, media.EgressPhasePausedReconfiguring, media.EgressPhasePausedReconnecting:
		return ErrStreamPaused
	case media.EgressPhaseStreaming:
		// 아래 Pause 호출로 실제 상태 전이를 수행한다.
	default:
		// idle과 reconnecting은 출력 형식이 확정됐거나 RTMP가 살아 있다는
		// 보장이 없으므로 일시 중단을 허용하지 않는다.
		return ErrStreamNotActive
	}
	if !egress.Pause() {
		switch egress.Status().Phase {
		case media.EgressPhaseStopped:
			return ErrStreamNotActive
		case media.EgressPhasePaused, media.EgressPhasePausedReconfiguring, media.EgressPhasePausedReconnecting:
			return ErrStreamPaused
		default:
			return ErrStreamNotActive
		}
	}
	return nil
}

func resumeEgress(egress streamPauseController) error {
	switch egress.Status().Phase {
	case media.EgressPhaseStopped:
		return ErrStreamNotActive
	case media.EgressPhasePaused:
		// 아래 Resume 호출로 실제 상태 전이를 수행한다.
	default:
		return ErrStreamNotPaused
	}
	if !egress.Resume() {
		if egress.Status().Phase == media.EgressPhaseStopped {
			return ErrStreamNotActive
		}
		return ErrStreamNotPaused
	}
	return nil
}

func (m *Manager) Delete(id, reason string) error {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		m.mu.Unlock()
		return ErrNotFound
	}
	delete(m.sessions, id)
	count := len(m.sessions)
	m.mu.Unlock()
	m.metrics.SetActiveSessions(count)
	s.close(reason, m.logger)
	return nil
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()
	m.metrics.SetActiveSessions(0)
	for _, s := range sessions {
		s.close("application_shutdown", m.logger)
	}
	// Wait for the FFmpeg pairs' graceful teardown so process exit does not
	// orphan children mid-shutdown (bounded slightly above the grace period).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.RLock()
		remaining := len(m.pipelines)
		m.mu.RUnlock()
		if remaining == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	m.mu.RLock()
	remaining := len(m.pipelines)
	m.mu.RUnlock()
	if remaining > 0 {
		m.logger.Warn("media pipelines still draining at shutdown", "remaining", remaining)
	}
}

// handleAudioTrack captures the publisher's Opus microphone and feeds it to the
// session's audio pipe for RTMP egress. Only the first Opus track is used;
// non-Opus or additional audio tracks are ignored (and counted). The read loop
// exits on EOF (track/PeerConnection closed) or session teardown, following the
// video path's ReadRTP semantics. No PLI/keyframe RTCP is sent for audio.
func (m *Manager) handleAudioTrack(ctx context.Context, s *Session, track *webrtc.TrackRemote) {
	codec := track.Codec()
	s.mu.Lock()
	pipe := s.audioPipe
	switch {
	case pipe == nil:
		// Audio egress disabled: preserve the prior ignore-and-count behaviour.
		s.ignoredTracks++
		s.UpdatedAt = time.Now().UTC()
		s.mu.Unlock()
		return
	case !strings.EqualFold(codec.MimeType, webrtc.MimeTypeOpus):
		s.ignoredTracks++
		s.mu.Unlock()
		m.logger.Warn("ignoring non-Opus audio track", "session_id", s.ID, "codec", codec.MimeType)
		return
	case s.audioTrackID != "":
		s.ignoredTracks++
		s.mu.Unlock()
		m.logger.Info("ignoring additional audio track", "session_id", s.ID, "track_id", track.ID())
		return
	}
	s.audioTrackID = track.ID()
	s.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()

	pipe.SetChannels(codec.Channels)
	m.logger.Info("received WebRTC audio track", "session_id", s.ID, "track_id", track.ID(),
		"codec", codec.MimeType, "clock_rate", codec.ClockRate, "channels", codec.Channels)

	go func() {
		for {
			packet, _, err := track.ReadRTP()
			if err != nil {
				if ctx.Err() == nil && !errors.Is(err, io.EOF) {
					m.logger.Error("audio RTP read failed", "session_id", s.ID, "error", err)
				}
				s.mu.Lock()
				if s.audioTrackID == track.ID() {
					s.audioTrackID = ""
				}
				s.mu.Unlock()
				return
			}
			pipe.WritePacket(packet)
		}
	}()
}

func (m *Manager) installHandlers(ctx context.Context, s *Session) {
	s.PC.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		// RTPReceiver.ReadRTCP runs Pion's receiver-report interceptors. Those
		// interceptors consume inbound Sender Reports and return Receiver Reports
		// with a LastSenderReport value, which lets the Pion load client calculate
		// RTCP RTT. The media loop reads RTP separately, so drain RTCP concurrently.
		go drainReceiverRTCP(receiver)
		if track.Kind() == webrtc.RTPCodecTypeAudio {
			m.handleAudioTrack(ctx, s, track)
			return
		}
		if track.Kind() != webrtc.RTPCodecTypeVideo {
			s.mu.Lock()
			s.ignoredTracks++
			s.UpdatedAt = time.Now().UTC()
			s.mu.Unlock()
			return
		}
		codec := media.VideoCodec(track.Codec().MimeType)
		s.mu.RLock()
		expectedCodec := s.VideoCodec
		s.mu.RUnlock()
		if !codec.Valid() || codec != expectedCodec {
			s.mu.Lock()
			s.ignoredTracks++
			s.mu.Unlock()
			m.logger.Warn("ignoring video track outside negotiated codec", "session_id", s.ID, "codec", track.Codec().MimeType, "expected_codec", expectedCodec)
			return
		}
		s.mu.Lock()
		if s.rawTrackID != "" && s.trackCancel != nil {
			// Replacement track (e.g. camera switch): stop the old pipeline and
			// re-wrap the new track into the same output track — matches Python's
			// live video-track replacement instead of ignoring it.
			m.logger.Info("replacing video track", "session_id", s.ID, "previous_track_id", s.rawTrackID, "new_track_id", track.ID())
			s.trackCancel()
		}
		trackCtx, trackCancel := context.WithCancel(ctx)
		s.trackCancel = trackCancel
		s.rawTrackID = track.ID()
		if s.Output != nil {
			s.processedTrackID = s.Output.ID()
		}
		s.UpdatedAt = time.Now().UTC()
		s.mu.Unlock()

		var aiStream media.AIStream
		if m.cfg.PrivacyMode == config.PrivacyModeReal {
			client := m.ai.Next()
			m.metrics.IncAITargetSession(client.Address())
			// The AI server requires a non-empty session scope. Use the
			// server-derived AI client ID so its per-user reference-face bucket
			// and stream identity cannot be selected through request metadata.
			aiStream = client.NewStream(trackCtx, s.AIClientID)
		}
		transcoder := media.NewFFmpegTranscoder(m.cfg.FFmpegPath, m.logger.With("session_id", s.ID), m.metrics, media.TranscoderOptions{
			Gate:           m.spawnGate,
			EncoderThreads: m.cfg.FFmpegEncoderThreads,
			WireFormat:     m.cfg.AIWireFormat,
			VideoCodec:     codec,
		})
		processor, err := media.NewProcessor(m.cfg.PrivacyMode, m.cfg.PrivacyFixedDelay, aiStream, m.metrics, m.logger.With("session_id", s.ID), m.cfg.AIWireFormat, m.cfg.AIFailurePolicy, m.cfg.AITimeoutLatchThreshold)
		if err != nil {
			m.logger.Error("create video processor failed", "session_id", s.ID, "error", err)
			return
		}
		s.mu.Lock()
		s.processor = processor
		// egress는 직접 들지 않고 세션의 슬롯을 통한다(#83): 트랙 교체로
		// 파이프라인이 재생성돼도, 방송 중 start/stop으로 egress가 갈려도
		// Enqueue 경로가 끊기지 않는다.
		egressSlot := s.egressSlot
		s.mu.Unlock()
		m.logger.Info("received WebRTC video track", "session_id", s.ID, "track_id", track.ID(), "codec", track.Codec().MimeType, "mode", m.cfg.PrivacyMode)
		trackID := track.ID()
		// The pipeline asks for a keyframe when the sample builder gives up on a
		// gap: the decoder has lost its reference, and every frame re-encoded
		// from that point carries the damage until the publisher sends a new one.
		requestKeyframe := func() {
			if err := s.PC.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())},
			}); err != nil {
				m.logger.Warn("keyframe request failed", "session_id", s.ID, "error", err)
			}
		}
		go func() {
			m.mu.Lock()
			m.pipelines[s.ID] = struct{}{}
			m.mu.Unlock()
			defer func() {
				m.mu.Lock()
				delete(m.pipelines, s.ID)
				m.mu.Unlock()
			}()
			media.RunTrack(trackCtx, m.logger.With("session_id", s.ID), track, s.Output, processor, transcoder, egressSlot, m.metrics, m.cfg.PrivacyMode, m.cfg.FrameQueueSize, requestKeyframe)
			s.mu.Lock()
			// Only clear if we are still the active track — a replacement may have
			// taken over while this old pipeline was draining.
			if s.rawTrackID == trackID {
				s.rawTrackID = ""
				s.processedTrackID = ""
				s.UpdatedAt = time.Now().UTC()
			}
			s.mu.Unlock()
		}()
	})

	s.PC.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		m.logger.Info("PeerConnection state changed", "session_id", s.ID, "connection_state", state.String())
		s.mu.Lock()
		now := time.Now().UTC()
		s.UpdatedAt = now
		if state == webrtc.PeerConnectionStateConnected {
			if !s.offerReceivedAt.IsZero() {
				s.Timing.OfferToConnectedMS = millisecondsPtr(now.Sub(s.offerReceivedAt))
			}
			if !s.answerCreatedAt.IsZero() {
				s.Timing.AnswerToConnectedMS = millisecondsPtr(now.Sub(s.answerCreatedAt))
			}
			if s.disconnectTimer != nil {
				s.disconnectTimer.Stop()
				s.disconnectTimer = nil
				m.metrics.IncReconnects()
			}
			s.wasConnected = true
		}
		if state == webrtc.PeerConnectionStateDisconnected && s.disconnectTimer == nil {
			s.disconnectTimer = time.AfterFunc(m.cfg.DisconnectedGracePeriod, func() {
				if s.PC.ConnectionState() == webrtc.PeerConnectionStateDisconnected {
					_ = m.Delete(s.ID, "peer_connection_disconnected_timeout")
				}
			})
		}
		s.mu.Unlock()
		if state == webrtc.PeerConnectionStateFailed {
			m.metrics.IncConnectionFailures()
			go func() { _ = m.Delete(s.ID, "peer_connection_failed") }()
		}
		if state == webrtc.PeerConnectionStateClosed {
			go func() { _ = m.Delete(s.ID, "peer_connection_closed") }()
		}
	})

	s.PC.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		m.logger.Info("ICE connection state changed", "session_id", s.ID, "ice_connection_state", state.String())
		if state != webrtc.ICEConnectionStateCompleted {
			return
		}
		s.mu.Lock()
		now := time.Now().UTC()
		if !s.offerReceivedAt.IsZero() {
			s.Timing.OfferToICECompletedMS = millisecondsPtr(now.Sub(s.offerReceivedAt))
		}
		if !s.answerCreatedAt.IsZero() {
			s.Timing.AnswerToICECompletedMS = millisecondsPtr(now.Sub(s.answerCreatedAt))
		}
		s.mu.Unlock()
	})
}

func (s *Session) Response() Response {
	s.mu.RLock()
	defer s.mu.RUnlock()
	response := Response{
		SessionID: s.ID,
		Status:    s.Status,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
		Metadata:  copyMetadata(s.Metadata),
		Timing:    s.Timing,
		Stream:    s.Stream,
	}
	response.Peer.ConnectionState = s.PC.ConnectionState().String()
	response.Peer.ICEConnectionState = s.PC.ICEConnectionState().String()
	response.Peer.SignalingState = s.PC.SignalingState().String()
	if s.rawTrackID != "" {
		response.Media.RawVideoTrack = &TrackState{ID: s.rawTrackID, Kind: "video", ReadyState: "live"}
	}
	if s.processedTrackID != "" {
		response.Media.ProcessedVideoTrack = &TrackState{ID: s.processedTrackID, Kind: "video", ReadyState: "live"}
	}
	response.Media.VideoSenderActive = s.Sender != nil
	response.Media.IgnoredTrackCount = s.ignoredTracks
	response.Media.UnsupportedTrackPolicy = "ignore_and_stop"
	if s.processor != nil {
		response.Media.AIFallbackActive = s.processor.FallbackActive()
	}
	if s.egress != nil {
		response.Stream = streamStateFromEgress(s.egress.Status(), s.rawTrackID != "", s.streamStopReason)
	}
	return response
}

// streamStateFromEgress는 egress 상태 스냅샷을 API 응답 계약(StreamState)으로
// 옮긴다.
func streamStateFromEgress(status media.EgressStatus, publisherActive bool, stopReason *string) StreamState {
	state := StreamState{
		Status:            string(status.Phase),
		StartedAt:         status.StartedAt,
		StoppedAt:         status.StoppedAt,
		UpdatedAt:         status.UpdatedAt,
		PublisherActive:   publisherActive,
		LastError:         status.LastError,
		StopReason:        stopReason,
		ReconnectAttempts: status.ReconnectAttempts,
		VideoWidth:        status.Width,
		VideoHeight:       status.Height,
		VideoFPS:          status.FPS,
		PausedAt:          status.PausedAt,
	}
	if status.TargetURL != "" {
		target := status.TargetURL
		state.TargetURL = &target
	}
	return state
}

func (s *Session) close(reason string, logger *slog.Logger) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.Status = "closing"
	if s.disconnectTimer != nil {
		s.disconnectTimer.Stop()
		s.disconnectTimer = nil
	}
	// 세션 ctx 취소로도 전파되지만, egress 종료가 세션 teardown의 일부임을
	// 명시하기 위해 상태를 먼저 stopped로 전이하고 전용 cancel을 직접 호출한다.
	// 이 순서로 WebRTC 종료·세션 삭제·로그아웃 모두 재구성 완료를 기다리지
	// 않고 같은 stop 전이와 RTMP 종료 경로를 사용한다.
	if s.egressCancel != nil {
		s.egress.Stop()
		s.egressCancel()
	}
	s.cancel()
	s.mu.Unlock()
	if err := s.PC.Close(); err != nil {
		logger.Error("close PeerConnection failed", "session_id", s.ID, "reason", reason, "error", err)
	}
	s.mu.Lock()
	s.Status = "closed"
	s.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()
	logger.Info("closed live session", "session_id", s.ID, "reason", reason)
}

func buildICEServers(cfg config.Config) []webrtc.ICEServer {
	servers := make([]webrtc.ICEServer, 0, 2)
	if len(cfg.STUNURLs) > 0 {
		servers = append(servers, webrtc.ICEServer{URLs: cfg.STUNURLs})
	}
	if len(cfg.TURNURLs) > 0 {
		servers = append(servers, webrtc.ICEServer{URLs: cfg.TURNURLs, Username: cfg.TURNUsername, Credential: cfg.TURNCredential})
	}
	return servers
}

func copyMetadata(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func millisecondsPtr(duration time.Duration) *int64 {
	value := duration.Milliseconds()
	return &value
}
