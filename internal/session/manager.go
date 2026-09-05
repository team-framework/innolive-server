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
	ErrNotFound                  = errors.New("session not found")
	ErrCapacityExceeded          = errors.New("session capacity exceeded")
	ErrNoVideoTrack              = errors.New("no video track is available")
	ErrStreamActive              = errors.New("stream egress is already active")
	ErrStreamNotActive           = errors.New("stream egress is not active")
	ErrStreamPaused              = errors.New("stream egress is already paused")
	ErrStreamNotPaused           = errors.New("stream egress is not paused")
	ErrRecoveryAttemptsExhausted = errors.New("WebRTC recovery attempts are exhausted")
	ErrStaleNegotiation          = errors.New("signaling message belongs to a stale negotiation")
	// 플랫폼 방송 라이프사이클(준비 → 라이브) 위반.
	ErrBroadcastPrepared    = errors.New("the platform broadcast is already prepared")
	ErrBroadcastNotPrepared = errors.New("the platform broadcast is not prepared")
	ErrBroadcastLive        = errors.New("the platform broadcast is already live")
	ErrBroadcastGoingLive   = errors.New("the platform broadcast is switching to live")
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
	// BroadcastPhase는 플랫폼 방송의 위치(idle/prepared/live)다. egress는
	// "준비만 된 방송"을 알 수 없어 egress phase에서 파생할 수 없다.
	BroadcastPhase BroadcastPhase `json:"broadcast_phase"`
}

// PeerRecoveryStatus는 WebRTC 입력 연결의 네트워크 복구 단계다. RTMP egress의
// 재연결 상태와 달리, 이 값은 같은 PeerConnection에서 ICE 후보를 다시 모으는
// 세션 계층의 수명만 나타낸다.
type PeerRecoveryStatus string

const (
	PeerRecoveryStatusIdle       PeerRecoveryStatus = "idle"
	PeerRecoveryStatusWaiting    PeerRecoveryStatus = "waiting"
	PeerRecoveryStatusRecovering PeerRecoveryStatus = "recovering"
)

// PeerConnectionState는 WebRTC 연결과 네트워크 복구 관측값을 함께 반환한다.
// StreamState와 분리하여 RTMP 장애와 입력 연결 장애를 API에서 혼동하지 않게 한다.
type PeerConnectionState struct {
	ConnectionState     string             `json:"connection_state"`
	ICEConnectionState  string             `json:"ice_connection_state"`
	SignalingState      string             `json:"signaling_state"`
	RecoveryStatus      PeerRecoveryStatus `json:"recovery_status"`
	ReconnectAttempts   int                `json:"reconnect_attempts"`
	RecoveryDeadline    *time.Time         `json:"recovery_deadline"`
	LastConnectionError *string            `json:"last_connection_error"`
}

// WebRTCRecoveryPolicy는 모든 클라이언트가 동일한 서버 복구 창 안에서 ICE
// restart를 시도하도록 /webrtc/config으로 전달하는 읽기 전용 계약이다.
type WebRTCRecoveryPolicy struct {
	WindowMS    int64 `json:"window_ms"`
	DebounceMS  int64 `json:"debounce_ms"`
	MaxAttempts int   `json:"max_attempts"`
}

type TrackState struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	ReadyState string `json:"ready_state"`
}

type Response struct {
	SessionID string              `json:"session_id"`
	Status    string              `json:"status"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
	Metadata  map[string]string   `json:"metadata"`
	Peer      PeerConnectionState `json:"peer_connection"`
	Timing    Timing              `json:"timing"`
	Media     struct {
		RawVideoTrack          *TrackState `json:"raw_video_track"`
		ProcessedVideoTrack    *TrackState `json:"processed_video_track"`
		VideoSenderActive      bool        `json:"video_sender_active"`
		IgnoredTrackCount      int         `json:"ignored_track_count"`
		UnsupportedTrackPolicy string      `json:"unsupported_track_policy"`
		AIFallbackActive       bool        `json:"ai_fallback_active"`
		AnonymizationEnabled   bool        `json:"anonymization_enabled"`
	} `json:"media"`
	Stream    StreamState               `json:"stream"`
	Broadcast *YouTubeBroadcastResponse `json:"broadcast"`
}

type Session struct {
	mu sync.RWMutex

	ID     string
	UserID uuid.UUID
	// GuestID is an opaque server-issued identifier for an unauthenticated
	// experience session. It is never returned in the public session response.
	GuestID    string
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
	// aiInputPaused는 방송 pause 의도를 보존한다. egress가 재구성·재연결 중인
	// 경우와 카메라 트랙이 교체되는 경우에도 새 카메라 프레임을 AI worker로
	// 보내지 않도록, egress의 순간 상태가 아니라 세션에 기록한다.
	aiInputPaused        bool
	anonymizationEnabled bool
	broadcast            *YouTubeBroadcastSettings
	platformBroadcast    *PlatformBroadcast
	broadcastPhase       BroadcastPhase
	// goLiveStopRequested는 라이브 전환 왕복 중에 들어온 중지 요청이다.
	// 이미 플랫폼으로 나간 전환 요청은 취소할 수 없으므로, 중지가 이겼다는
	// 사실만 남겨두고 전환 결과를 받은 쪽이 방송을 종료시킨다.
	goLiveStopRequested bool
	ignoredTracks       int
	offerReceivedAt     time.Time
	answerCreatedAt     time.Time
	pendingICE          []webrtc.ICECandidateInit
	activeNegotiationID string
	// localCandidateRoutes는 Pion이 만든 candidate의 ufrag를 해당 candidate를
	// 수집한 signaling generation에 연결한다. Pion의 내부 queue는 callback을
	// 늦게 실행할 수 있으므로, 가변 "현재 generation"으로 후보를 표기하면 안 된다.
	localCandidateRoutes     map[string]localCandidateRoute
	localCandidateRouteOrder []string
	negotiationMu            sync.Mutex
	cancel                   context.CancelFunc
	trackCancel              context.CancelFunc
	recovery                 peerRecovery
	wasConnected             bool
	closed                   bool
	// lastActivityAt은 소유자가 마지막으로 세션을 사용한 시각이다. 미협상 회수는
	// 이 시각을 기준으로 다시 재므로, 방송 설정을 채우는 동안에는 회수되지 않는다(#147).
	lastActivityAt time.Time
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
	// deleting tracks teardown calls that have removed a session from sessions
	// but have not yet reached the server-owned cleanup hook. Graceful shutdown
	// waits for these before closing dependencies used by that hook.
	deleting sync.WaitGroup
	// pipelines는 아직 살아 있는 미디어 파이프라인(FFmpeg 쌍)을 가진 세션을
	// 추적한다. map에서 제거됐지만 아직 종료되지 않은 세션도 포함하며, 종료
	// 유예 시간 동안 재연결 반복으로 프로세스가 중복 배정되지 않도록
	// MaxSessions 사용량에 계속 포함한다.
	pipelines map[string]struct{}

	// broadcastCleanup은 세션이 끝날 때 남은 플랫폼 방송을 치우는 훅이다.
	// 세션 계층은 플랫폼 provider를 몰라야 하므로(역방향 결합) 서버가 조립
	// 시점에 주입한다. nil이면 정리를 하지 않는다. phase는 정리 방법을
	// 가르는 값이다 — prepared는 삭제, live는 즉시 종료다.
	broadcastCleanup func(userID uuid.UUID, broadcast PlatformBroadcast, phase BroadcastPhase)
	sessionCleanup   func(*Session)
}

// SetBroadcastCleanup은 세션 종료 시 준비된 플랫폼 방송을 치우는 훅을 등록한다.
// WebRTC 실패·유예 시간 초과·로그아웃·명시적 삭제가 모두 Delete로 모이므로
// 훅 하나로 전부 덮인다. 서버 조립 직후 한 번만 호출한다.
func (m *Manager) SetBroadcastCleanup(cleanup func(userID uuid.UUID, broadcast PlatformBroadcast, phase BroadcastPhase)) {
	m.broadcastCleanup = cleanup
}

// SetSessionCleanup registers server-owned cleanup that applies to every
// termination path (explicit deletion, timeouts, and peer failures).
func (m *Manager) SetSessionCleanup(cleanup func(*Session)) { m.sessionCleanup = cleanup }

// cleanupPlatformBroadcast는 세션에 남은 플랫폼 방송을 치운다. 라이브였던
// 방송도 여기서 끝낸다 — autoStop을 기다리면 그 사이(실측 약 1분)에 시작한
// 다음 방송과 겹쳐 라이브가 둘이 된다. 플랫폼 왕복이 세션 teardown을 붙잡지
// 않도록 별도 고루틴에서 돌린다.
func (m *Manager) cleanupPlatformBroadcast(s *Session) {
	s.mu.Lock()
	broadcast, phase := takeBroadcastLocked(s)
	s.mu.Unlock()
	m.disposeBroadcast(s.UserID, broadcast, phase)
}

// takeBroadcastLocked는 정리해야 할 방송을 세션에서 떼어내고 단계를 idle로
// 되돌린다. 라이브 전환 왕복 중이면 손대지 않는다 — 이미 나간 요청의 결과를
// 받는 쪽이 마무리해야 하기 때문이다. Session.mu를 가진 호출자만 쓴다.
func takeBroadcastLocked(s *Session) (PlatformBroadcast, BroadcastPhase) {
	phase := s.broadcastPhase
	if phase == BroadcastPhaseGoingLive {
		return PlatformBroadcast{}, BroadcastPhaseIdle
	}
	var broadcast PlatformBroadcast
	if s.platformBroadcast != nil && (phase == BroadcastPhasePrepared || phase == BroadcastPhaseLive) {
		broadcast = *s.platformBroadcast
	} else {
		phase = BroadcastPhaseIdle
	}
	s.platformBroadcast = nil
	s.broadcastPhase = BroadcastPhaseIdle
	return broadcast, phase
}

// disposeBroadcast는 떼어낸 방송을 플랫폼에서 치운다. 치울 것이 없으면 아무
// 일도 하지 않는다.
func (m *Manager) disposeBroadcast(userID uuid.UUID, broadcast PlatformBroadcast, phase BroadcastPhase) {
	if m.broadcastCleanup == nil || phase == BroadcastPhaseIdle || broadcast.BroadcastID == "" {
		return
	}
	go m.broadcastCleanup(userID, broadcast, phase)
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
		// 모든 ICE UDP 트래픽을 단일 포트에 multiplex한다. 기본값처럼 미디어를
		// ephemeral 포트 범위에 분산하면 흐름마다 NAT binding이 하나씩 생겨,
		// 다중 세션에서 가정용 공유기 NAT 테이블을 소진한다. 클라이언트가 다른
		// 장비에서 실행될 때 심한 RTP 패킷 손실로 관찰됐다. 단일 mux 포트는 이를
		// binding 하나로 줄이고 방화벽에서도 UDP 포트 하나만 열면 된다.
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

func (m *Manager) WebRTCRecoveryPolicy() WebRTCRecoveryPolicy {
	return WebRTCRecoveryPolicy{
		WindowMS:    recoveryWindow(m.cfg).Milliseconds(),
		DebounceMS:  recoveryDebounce(m.cfg).Milliseconds(),
		MaxAttempts: recoveryMaxAttempts(m.cfg),
	}
}

// Capacity는 용량을 사용하는 세션 수(실행 중 + 예약)와 설정한 제한을 반환한다.
// 제한값 0은 무제한을 뜻한다.
func (m *Manager) Capacity() (active, limit int) {
	m.mu.RLock()
	active = len(m.sessions) + m.pending
	m.mu.RUnlock()
	return active, m.cfg.MaxSessions
}

// capacityInUseLocked는 실행 중인 세션, 진행 중인 예약, 종료 중인 FFmpeg 쌍이
// 남은 삭제 세션의 orphaned pipeline을 센다. 호출자는 m.mu를 잡고 있어야 한다.
func (m *Manager) capacityInUseLocked() int {
	inUse := len(m.sessions) + m.pending
	for id := range m.pipelines {
		if _, live := m.sessions[id]; !live {
			inUse++
		}
	}
	return inUse
}

// Create는 새 세션을 만들고 평문 owner token과 함께 반환한다. token은 여기서
// 정확히 한 번만 반환하며, 서버는 생성 응답으로 생성자에게 전달한 뒤 다시
// 노출해서는 안 된다.
func (m *Manager) Create(metadata map[string]string) (*Session, string, error) {
	return m.CreateForUser(uuid.Nil, metadata)
}

// CreateForUser는 userID가 소유하는 세션을 만든다. AI client ID는 요청 metadata로
// 받지 않고 여기서 계산해, 얼굴 whitelist와 미디어 스트림이 항상 같은 인증 범위를
// 사용하게 한다.
func (m *Manager) CreateForUser(userID uuid.UUID, metadata map[string]string) (*Session, string, error) {
	return m.create(userID, "", metadata)
}

// CreateForGuest creates a session owned by a server-issued guest identity.
// The identity is kept separate from UserID so guest routes can never satisfy
// authenticated-user ownership checks by using uuid.Nil.
func (m *Manager) CreateForGuest(guestID string, metadata map[string]string) (*Session, string, error) {
	if strings.TrimSpace(guestID) == "" {
		return nil, "", errors.New("guest ID is required")
	}
	return m.create(uuid.Nil, guestID, metadata)
}

func (m *Manager) create(userID uuid.UUID, guestID string, metadata map[string]string) (*Session, string, error) {
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
	aiClientID := AIClientIDForUser(userID)
	if guestID != "" {
		aiClientID = "guest:" + guestID + ":session:" + id
	}
	s := &Session{
		ID:                   id,
		UserID:               userID,
		GuestID:              guestID,
		AIClientID:           aiClientID,
		CreatedAt:            now,
		UpdatedAt:            now,
		lastActivityAt:       now,
		Metadata:             copyMetadata(metadata),
		Status:               "active",
		PC:                   pc,
		Stream:               StreamState{Status: "idle", UpdatedAt: now, BroadcastPhase: BroadcastPhaseIdle},
		broadcastPhase:       BroadcastPhaseIdle,
		anonymizationEnabled: m.cfg.PrivacyMode == config.PrivacyModeReal,
		ownerHash:            ownerHash,
		cancel:               cancel,
		recovery:             peerRecovery{status: PeerRecoveryStatusIdle},
	}
	s.baseCtx = ctx
	s.egressSlot = media.NewEgressSlot()
	// audio pipe는 트랙 도착 순서와 무관하게 미리 만든다. 영상 또는 오디오 트랙의
	// OnTrack 중 무엇이 먼저 실행돼도 egress가 연결할 수 있다. 마이크 트랙이
	// 입력을 공급하고 egress가 pipe를 연결할 때까지는 샘플을 버리며 유휴 상태다.
	// 사용자별 송출(#83)에서는 세션 생성 시점에 송출 여부를 알 수 없으므로 오디오
	// egress 설정만 보고 만든다.
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

// GuestCount returns the number of active sessions owned by guests.
func (m *Manager) GuestCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, liveSession := range m.sessions {
		if liveSession.GuestID != "" {
			count++
		}
	}
	return count
}

// AIClientIDForUser는 인증된 사용자가 사용하는 유일한 whitelist bucket 식별자다.
// 접두사는 호출자가 제어하는 선택자를 노출하지 않으면서 legacy global bucket
// (빈 client ID)과 구분한다.
func AIClientIDForUser(userID uuid.UUID) string {
	return "user:" + userID.String()
}

// maxNegotiationExtension은 미협상 세션이 활동으로 회수를 미룰 수 있는 상한이다.
// 마지막 활동 기준으로 타임아웃을 다시 재면(#147) 방송 설정 저장을 반복하는 것만으로
// 세션이 무기한 살아남아 MAX_SESSIONS 슬롯을 점유할 수 있어, 생성 이후 이 시간이
// 지나면 더 연장하지 않는다. 폼을 채우는 데 걸리는 시간보다 넉넉하고, 상한에 걸려
// 회수돼도 클라이언트는 새 세션으로 이어간다.
var maxNegotiationExtension = 10 * time.Minute

// reapUnnegotiated는 SESSION_NEGOTIATION_TIMEOUT 안에 협상을 시작하지 않은
// 세션을 해제한다. POST /sessions만 호출해 MAX_SESSIONS 슬롯을 무기한 점유하지
// 못하게 한다.
func (m *Manager) reapUnnegotiated(id string) {
	s, err := m.Get(id)
	if err != nil {
		return
	}
	s.mu.RLock()
	bare := s.offerReceivedAt.IsZero() && !s.wasConnected && !s.closed
	idleFor := time.Since(s.lastActivityAt)
	s.mu.RUnlock()
	if !bare {
		return
	}
	// 방송 설정을 저장하는 소유자는 세션을 방치한 것이 아니다(#147). 마지막 활동
	// 이후로 타임아웃을 다시 재, 폼을 채우는 동안 회수되지 않게 한다. 활동이 없는
	// 세션은 원래대로 timeout 뒤에 슬롯을 돌려준다. 연장은 maxNegotiationExtension까지만
	// 허용해 협상 없는 세션의 수명 상한을 유지한다.
	if remaining := m.cfg.NegotiationTimeout - idleFor; remaining > 0 {
		if extendable := maxNegotiationExtension - time.Since(s.CreatedAt); extendable > 0 {
			time.AfterFunc(min(remaining, extendable), func() { m.reapUnnegotiated(id) })
			return
		}
		m.logger.Info("unnegotiated session exceeded extension limit", "session_id", id,
			"limit", maxNegotiationExtension)
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

// VerifyOwner는 세션을 찾고 호출자가 소유자인지 확인한다. 세션이 없으면
// ErrNotFound를, token이 일치하지 않으면 ErrUnauthorized를 반환한다. 세션 인증을
// 끈 경우(로컬 개발 전용)에는 token을 검사하지 않지만 세션은 존재해야 한다.
// HTTP middleware와 WebRTC signaling 경로가 함께 사용하는 단일 관문이다.
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
	}, outputURL, s.audioPipe, m.cfg.EgressLatencyLog, m.cfg.EgressAudioOffset, m.cfg.EgressVideoBitrate, m.cfg.EgressVideoSize)
	if s.audioPipe != nil {
		s.audioPipe.SetMuted(false)
	}
	s.setAIInputPaused(false)
	s.egress = egress
	s.egressCancel = egressCancel
	s.streamStopReason = nil
	s.egressSlot.Set(egress)
	s.UpdatedAt = time.Now().UTC()
	go m.runEgress(s, egress, egressCtx)
	m.logger.Info("RTMP egress started", "session_id", s.ID, "url", egress.Status().TargetURL)
	return s, nil
}

// runEgress는 egress의 자체 종료를 세션 슬롯 정리와 연결한다. Run 고루틴이
// 재연결 예산을 소진해 끝난 경우에도 세션과 WebRTC 미리보기는 살아 있어야 하므로,
// 이 함수는 세션을 지우지 않고 해당 egress만 슬롯에서 분리한다.
func (m *Manager) runEgress(s *Session, egress *media.RTMPEgress, egressCtx context.Context) {
	egress.Run(egressCtx)
	status := egress.Status()

	s.mu.Lock()
	if s.egress != egress {
		s.mu.Unlock()
		return
	}
	// 같은 세션에서 새 방송이 먼저 시작된 경우에는 ClearIf가 새 egress를
	// 지우지 않는다. 이 비교·교환은 트랙 파이프라인과의 경계를 원자적으로
	// 유지한다.
	s.egressSlot.ClearIf(egress)
	if s.egressCancel != nil {
		s.egressCancel()
		s.egressCancel = nil
	}
	if !s.closed {
		// pause 상태에서 재연결 예산이 소진된 경우에도 미리보기·AI 파이프라인은
		// 계속 동작해야 하므로 egress 종료와 함께 AI 입력 차단을 해제한다.
		s.setAIInputPaused(false)
	}
	// egress가 스스로 끝난 경우에도 플랫폼 방송과 단계를 정리한다. 그러지
	// 않으면 단계가 live에 남아 설정 변경이 영구히 409로 막히고, 유튜브 쪽
	// 방송도 다음 송출과 겹친다.
	broadcast, phase := takeBroadcastLocked(s)
	s.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()
	m.disposeBroadcast(s.UserID, broadcast, phase)

	if status.StopReason != nil {
		lastError := ""
		if status.LastError != nil {
			lastError = *status.LastError
		}
		m.logger.Warn("RTMP egress stopped after terminal recovery failure",
			"session_id", s.ID,
			"stop_reason", *status.StopReason,
			"reconnect_attempts", status.ReconnectAttempts,
			"last_error", lastError)
	}
}

// StopStream은 egress만 종료한다 — 세션과 뷰어(WebRTC) 송출은 유지된다.
// 두 번째 반환값은 호출자가 플랫폼에서 지워야 할 방송이다(없으면 zero value와
// false). 중지 시점의 단계 판정과 상태 전이가 갈라지면 라이브 전환과 경합하므로
// 한 번의 잠금 안에서 함께 정한다.
// 플랫폼 방송은 autoStop(송출 중단 약 1분 후 반영, 실측 57.6초)을 기다리지
// 않고 여기서 끝낸다 — 그 사이에 다음 방송을 시작하면 재사용 스트림 키를 통해
// 라이브가 둘이 되기 때문이다.
func (m *Manager) StopStream(id string) (*Session, PlatformBroadcast, BroadcastPhase, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, PlatformBroadcast{}, BroadcastPhaseIdle, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.egress == nil || s.streamStopReason != nil || s.egress.Status().Phase == media.EgressPhaseStopped {
		return nil, PlatformBroadcast{}, BroadcastPhaseIdle, ErrStreamNotActive
	}
	s.egressSlot.Clear()
	s.egress.Stop()
	s.egressCancel()
	// stop은 egress만 끝내고 WebRTC 처리 파이프라인은 유지하는 기존 계약을
	// 보존한다. pause 상태에서 stop한 경우에도 이후 처리 프레임은 다시 AI로
	// 보낼 수 있게 차단을 해제한다.
	s.setAIInputPaused(false)
	reason := "user_requested"
	s.streamStopReason = &reason
	// 라이브 전환 왕복 중이면 상태를 지우지 않는다 — 전환 결과를 받은 쪽이
	// 방송을 종료시켜야 하므로 중지가 요청됐다는 사실만 남긴다.
	if s.broadcastPhase == BroadcastPhaseGoingLive {
		s.goLiveStopRequested = true
		s.UpdatedAt = time.Now().UTC()
		m.logger.Info("RTMP egress stopped", "session_id", s.ID, "reason", reason)
		return s, PlatformBroadcast{}, BroadcastPhaseIdle, nil
	}
	broadcast, phase := takeBroadcastLocked(s)
	s.UpdatedAt = time.Now().UTC()
	m.logger.Info("RTMP egress stopped", "session_id", s.ID, "reason", reason)
	return s, broadcast, phase, nil
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
	s.setAIInputPaused(true)
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
	s.setAIInputPaused(false)
	s.UpdatedAt = time.Now().UTC()
	m.logger.Info("RTMP egress resumed", "session_id", s.ID)
	return s, nil
}

// SetAnonymizationEnabled는 WebRTC·RTMP 송출과 독립적으로 AI 처리만 켜고 끈다.
// 꺼져 있는 동안에는 원본 프레임이 두 출력으로 그대로 나가고, 빠른 재활성화를
// 위해 기존 gRPC bidi stream은 열어 둔다.
func (m *Manager) SetAnonymizationEnabled(id string, enabled bool) (*Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrNotFound
	}
	s.anonymizationEnabled = enabled
	if s.processor != nil {
		s.processor.SetAnonymizationEnabled(enabled)
	}
	s.UpdatedAt = time.Now().UTC()
	m.logger.Info("anonymization changed", "session_id", s.ID, "enabled", enabled)
	return s, nil
}

func pauseEgress(egress streamPauseController) error {
	switch egress.Status().Phase {
	case media.EgressPhaseStopped:
		return ErrStreamNotActive
	case media.EgressPhasePaused, media.EgressPhasePausedReconfiguring, media.EgressPhasePausedReconnecting:
		return ErrStreamPaused
	case media.EgressPhaseStreaming, media.EgressPhaseReconfiguring:
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

// setAIInputPaused는 Session.mu를 가진 호출자만 사용한다. Processor는 내부
// 잠금으로 진행 중인 AI 호출을 기다린 뒤 stream을 닫으므로, pause API가
// 성공으로 반환된 뒤에는 새 카메라 프레임이 AI worker에 전달되지 않는다.
func (s *Session) setAIInputPaused(paused bool) {
	s.aiInputPaused = paused
	if s.processor == nil {
		return
	}
	if paused {
		s.processor.SuspendAIInput()
		return
	}
	s.processor.ResumeAIInput()
}

func (m *Manager) Delete(id, reason string) error {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		m.mu.Unlock()
		return ErrNotFound
	}
	// Register before removing the session so CloseAll followed by
	// WaitForDeletes cannot miss a teardown already in progress.
	m.deleting.Add(1)
	delete(m.sessions, id)
	count := len(m.sessions)
	m.mu.Unlock()
	defer m.deleting.Done()
	m.metrics.SetActiveSessions(count)
	// close가 phase를 건드리지는 않지만, 정리 대상 판단은 닫기 전에 읽는다.
	m.cleanupPlatformBroadcast(s)
	s.close(reason, m.logger)
	if m.sessionCleanup != nil {
		m.sessionCleanup(s)
	}
	return nil
}

// WaitForDeletes waits for teardowns that were already in flight when
// CloseAll removed the remaining sessions. It is used during graceful process
// shutdown before closing server-owned cleanup dependencies such as the AI
// whitelist client.
func (m *Manager) WaitForDeletes(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		m.deleting.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
		m.cleanupPlatformBroadcast(s)
		s.close("application_shutdown", m.logger)
		if m.sessionCleanup != nil {
			m.sessionCleanup(s)
		}
	}
	// 종료 중인 FFmpeg 쌍이 정상적으로 끝날 때까지 기다려, shutdown 중 프로세스가
	// 자식 프로세스를 고아로 남기지 않게 한다. 대기 시간은 grace period보다 조금
	// 길게 제한한다.
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

// handleAudioTrack은 송출자의 Opus 마이크를 받아 RTMP egress용 세션 audio pipe에
// 공급한다. 첫 번째 Opus 트랙만 사용하며, Opus가 아니거나 추가된 오디오 트랙은
// 무시하고 집계한다. 읽기 루프는 영상 경로의 ReadRTP 의미와 같이 EOF(트랙 또는
// PeerConnection 종료)나 세션 종료 시 끝난다. 오디오에는 PLI/keyframe RTCP를
// 보내지 않는다.
func (m *Manager) handleAudioTrack(ctx context.Context, s *Session, track *webrtc.TrackRemote) {
	codec := track.Codec()
	s.mu.Lock()
	pipe := s.audioPipe
	switch {
	case pipe == nil:
		// Audio egress를 끈 경우 기존의 무시·집계 동작을 유지한다.
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
	s.PC.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		s.dispatchLocalICECandidate(candidate)
	})

	s.PC.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		// RTPReceiver.ReadRTCP는 Pion의 receiver-report interceptor를 실행한다.
		// interceptor는 들어온 Sender Report를 소비하고 LastSenderReport 값이 든
		// Receiver Report를 반환해 Pion load client가 RTCP RTT를 계산하게 한다.
		// 미디어 루프는 RTP를 따로 읽으므로 RTCP는 동시에 비운다.
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
			// 교체 트랙(예: 카메라 전환)이면 기존 파이프라인을 중지하고 새 트랙을
			// 같은 출력 트랙으로 다시 감싼다. 무시하지 않고 Python의 실시간 영상
			// 트랙 교체 동작과 맞춘다.
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
			// AI 서버는 비어 있지 않은 세션 scope를 요구한다. 서버가 계산한 AI
			// client ID를 사용해 요청 metadata로 사용자별 reference-face bucket과
			// 스트림 식별자를 선택할 수 없게 한다.
			aiStream = client.NewStream(trackCtx, s.AIClientID)
		}
		transcoder := media.NewFFmpegTranscoder(m.cfg.FFmpegPath, m.logger.With("session_id", s.ID), m.metrics, media.TranscoderOptions{
			Gate:           m.spawnGate,
			EncoderThreads: m.cfg.FFmpegEncoderThreads,
			WireFormat:     m.cfg.AIWireFormat,
			VideoCodec:     codec,
			PinLongEdge:    uint16(m.cfg.DecoderPinLongEdge),
		})
		processor, err := media.NewProcessor(m.cfg.PrivacyMode, m.cfg.PrivacyFixedDelay, aiStream, m.metrics, m.logger.With("session_id", s.ID), m.cfg.AIWireFormat, m.cfg.AIFailurePolicy, m.cfg.AITimeoutLatchThreshold)
		if err != nil {
			m.logger.Error("create video processor failed", "session_id", s.ID, "error", err)
			return
		}
		s.mu.Lock()
		// 새 Processor를 세션에 공개하기 전에 현재 제어 상태를 먼저 적용한다.
		// 그렇지 않으면 토글 요청이 이 새 Processor를 갱신한 직후, 여기서 읽은
		// 오래된 상태가 다시 덮어쓰는 경합이 생길 수 있다.
		if s.aiInputPaused {
			processor.SuspendAIInput()
		}
		processor.SetAnonymizationEnabled(s.anonymizationEnabled)
		s.processor = processor
		// egress는 직접 들지 않고 세션의 슬롯을 통한다(#83): 트랙 교체로
		// 파이프라인이 재생성돼도, 방송 중 start/stop으로 egress가 갈려도
		// Enqueue 경로가 끊기지 않는다.
		egressSlot := s.egressSlot
		s.mu.Unlock()
		m.logger.Info("received WebRTC video track", "session_id", s.ID, "track_id", track.ID(), "codec", track.Codec().MimeType, "mode", m.cfg.PrivacyMode)
		trackID := track.ID()
		// sample builder가 gap 복구를 포기하면 파이프라인은 keyframe을 요청한다.
		// decoder가 참조 프레임을 잃었으므로 송출자가 새 참조를 보낼 때까지 그
		// 이후 재인코딩 프레임마다 손상이 이어지기 때문이다.
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
			media.RunTrack(
				trackCtx,
				m.logger.With("session_id", s.ID),
				track,
				s.Output,
				processor,
				transcoder,
				egressSlot,
				m.metrics,
				m.cfg.PrivacyMode,
				m.cfg.FrameQueueSize,
				requestKeyframe,
				func() { m.noteProcessedMediaFrame(s) },
			)
			s.mu.Lock()
			// 아직 활성 트랙일 때만 비운다. 이전 파이프라인을 비우는 사이 새
			// 교체 트랙이 활성화됐을 수 있다.
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
			s.wasConnected = true
		}
		s.mu.Unlock()
		switch state {
		case webrtc.PeerConnectionStateConnected:
			m.connectedDuringRecovery(s)
		case webrtc.PeerConnectionStateDisconnected:
			m.scheduleDisconnectedRecovery(s)
		case webrtc.PeerConnectionStateFailed:
			m.metrics.IncConnectionFailures()
			s.mu.RLock()
			wasConnected := s.wasConnected
			s.mu.RUnlock()
			if wasConnected {
				m.beginRecovery(s, 0, "peer_connection_failed")
			} else {
				go func() { _ = m.Delete(s.ID, "peer_connection_failed") }()
			}
		}
		if state == webrtc.PeerConnectionStateClosed {
			// Session.close가 직접 닫은 경우에는 이미 정리 중이다. 외부에서 닫힌
			// 경우에만 세션 정리를 예약한다.
			s.mu.RLock()
			closed := s.closed
			s.mu.RUnlock()
			if !closed {
				go func() { _ = m.Delete(s.ID, "peer_connection_closed") }()
			}
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
	response.Peer.RecoveryStatus = s.recovery.status
	response.Peer.ReconnectAttempts = s.recovery.attempts
	if s.recovery.deadline != nil {
		deadline := *s.recovery.deadline
		response.Peer.RecoveryDeadline = &deadline
	}
	if s.recovery.lastError != nil {
		lastError := *s.recovery.lastError
		response.Peer.LastConnectionError = &lastError
	}
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
	response.Media.AnonymizationEnabled = s.anonymizationEnabled
	if s.broadcast != nil {
		broadcast := s.broadcast.response()
		response.Broadcast = &broadcast
	}
	if s.egress != nil {
		response.Stream = streamStateFromEgress(s.egress.Status(), s.rawTrackID != "", s.streamStopReason)
	}
	response.Stream.BroadcastPhase = s.broadcastPhase
	return response
}

// streamStateFromEgress는 egress 상태 스냅샷을 API 응답 계약(StreamState)으로
// 옮긴다.
func streamStateFromEgress(status media.EgressStatus, publisherActive bool, stopReason *string) StreamState {
	effectiveStopReason := stopReason
	if effectiveStopReason == nil && status.StopReason != nil {
		reason := string(*status.StopReason)
		effectiveStopReason = &reason
	}
	state := StreamState{
		Status:            string(status.Phase),
		StartedAt:         status.StartedAt,
		StoppedAt:         status.StoppedAt,
		UpdatedAt:         status.UpdatedAt,
		PublisherActive:   publisherActive,
		LastError:         status.LastError,
		StopReason:        effectiveStopReason,
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
	s.cancelRecoveryLocked()
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
