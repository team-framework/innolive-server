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
	"github.com/pion/webrtc/v4"
)

var (
	ErrNotFound         = errors.New("session not found")
	ErrCapacityExceeded = errors.New("session capacity exceeded")
)

const youtubeIngestURL = "rtmp://a.rtmp.youtube.com/live2/"

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
	// The audio pipe is created up front (decoupled from track arrival order)
	// so the egress can attach it regardless of whether the video or the audio
	// track's OnTrack fires first. It idles, dropping samples, until a mic track
	// feeds it and the egress attaches a pipe.
	if m.cfg.EnableAudioEgress && m.cfg.YoutubeStreamKey != "" {
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

// CloseUserSessions tears down every live session belonging to a withdrawn
// user. Session ownership is in-memory, so this complements the database
// account-status check that rejects all future API requests.
func (m *Manager) CloseUserSessions(userID uuid.UUID) {
	m.mu.RLock()
	ids := make([]string, 0)
	for id, liveSession := range m.sessions {
		if liveSession.UserID == userID {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range ids {
		if err := m.Delete(id, "user_withdrawal"); err != nil && !errors.Is(err, ErrNotFound) {
			m.logger.Warn("close withdrawn user session failed", "session_id", id, "user_id", userID, "error", err)
		}
	}
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
		s.mu.Unlock()
		var egress *media.RTMPEgress
		if m.cfg.YoutubeStreamKey != "" {
			egress = media.NewRTMPEgress(m.cfg.FFmpegPath, m.logger.With("session_id", s.ID), m.metrics, media.TranscoderOptions{
				Gate:       m.spawnGate,
				WireFormat: m.cfg.AIWireFormat,
			}, youtubeIngestURL+m.cfg.YoutubeStreamKey, s.audioPipe, m.cfg.EgressLatencyLog, m.cfg.EgressAudioOffset)
			go egress.Run(trackCtx)
			m.logger.Info("YouTube RTMP egress enabled", "session_id", s.ID, "url", youtubeIngestURL+"****")
		}
		m.logger.Info("received WebRTC video track", "session_id", s.ID, "track_id", track.ID(), "codec", track.Codec().MimeType, "mode", m.cfg.PrivacyMode)
		trackID := track.ID()
		go func() {
			m.mu.Lock()
			m.pipelines[s.ID] = struct{}{}
			m.mu.Unlock()
			defer func() {
				m.mu.Lock()
				delete(m.pipelines, s.ID)
				m.mu.Unlock()
			}()
			media.RunTrack(trackCtx, m.logger.With("session_id", s.ID), track, s.Output, processor, transcoder, egress, m.metrics, m.cfg.PrivacyMode, m.cfg.FrameQueueSize)
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
	return response
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
