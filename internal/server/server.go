package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"strings"
	"time"

	"github.com/google/uuid"

	"inno-live-server/internal/ai"
	"inno-live-server/internal/auth"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/origin"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"
)

const maxJSONBody = 1 << 20

// 방송 설정은 2MB 썸네일을 base64로 싣는다(약 1.34배 팽창) — 그 경로만
// 본문 한계를 따로 둔다.
const maxBroadcastBody = 4 << 20

// platformCleanupTimeout은 요청 컨텍스트가 끝난 뒤 하는 플랫폼 정리 호출의
// 한계다 — 요청이 취소·완료돼도 만들어둔 방송은 지워야 한다.
const platformCleanupTimeout = 10 * time.Second

type Server struct {
	cfg              config.Config
	logger           *slog.Logger
	metrics          *metrics.Registry
	sessions         *session.Manager
	ai               *ai.Pool
	references       *referenceStore
	origins          origin.Config
	streaming        map[auth.StreamingProvider]streaming.Provider
	authenticateUser func(context.Context, string) (uuid.UUID, error)
	handler          http.Handler
}

func New(
	cfg config.Config,
	logger *slog.Logger,
	registry *metrics.Registry,
	sessions *session.Manager,
	aiPool *ai.Pool,
	origins origin.Config,
	requireUser func(http.Handler) http.Handler,
	streamingProviders map[auth.StreamingProvider]streaming.Provider,
	userAuthenticators ...func(context.Context, string) (uuid.UUID, error),
) *Server {
	if requireUser == nil {
		requireUser = func(next http.Handler) http.Handler { return next }
	}
	s := &Server{
		cfg:        cfg,
		logger:     logger,
		metrics:    registry,
		sessions:   sessions,
		ai:         aiPool,
		references: newReferenceStore(cfg.ReferenceStorePath, cfg.AIMeImagePath != ""),
		origins:    origins,
		streaming:  streamingProviders,
	}
	if len(userAuthenticators) > 0 {
		s.authenticateUser = userAuthenticators[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.Handle("GET /webrtc/config", requireUser(http.HandlerFunc(s.handleWebRTCConfig)))
	mux.Handle("POST /sessions", requireUser(http.HandlerFunc(s.handleCreateSession)))
	// Every session-scoped route goes through requireSessionOwner so ownership
	// is enforced structurally. The list endpoint is intentionally removed: it
	// leaked every active session_id, defeating the token model.
	mux.Handle("GET /sessions/{session_id}", requireUser(s.requireSessionOwner(s.handleGetSession)))
	mux.Handle("DELETE /sessions/{session_id}", requireUser(s.requireSessionOwner(s.handleDeleteSession)))
	mux.Handle("POST /sessions/{session_id}/stream/prepare", requireUser(s.requireSessionOwner(s.handlePrepareStream)))
	mux.Handle("POST /sessions/{session_id}/stream/golive", requireUser(s.requireSessionOwner(s.handleGoLive)))
	mux.Handle("POST /sessions/{session_id}/stream/pause", requireUser(s.requireSessionOwner(s.handlePauseStream)))
	mux.Handle("POST /sessions/{session_id}/stream/resume", requireUser(s.requireSessionOwner(s.handleResumeStream)))
	mux.Handle("POST /sessions/{session_id}/stream/stop", requireUser(s.requireSessionOwner(s.handleStopStream)))
	mux.Handle("PUT /sessions/{session_id}/broadcast", requireUser(s.requireSessionOwner(s.handlePutBroadcast)))
	mux.Handle("PATCH /sessions/{session_id}/anonymization", requireUser(s.requireSessionOwner(s.handlePatchAnonymization)))
	mux.Handle("GET /reference-face", requireUser(http.HandlerFunc(s.handleGetReferenceFace)))
	mux.Handle("POST /reference-face", requireUser(http.HandlerFunc(s.handlePostReferenceFace)))
	mux.Handle("DELETE /reference-face", requireUser(http.HandlerFunc(s.handleDeleteReferenceFace)))
	mux.Handle("DELETE /reference-face/{face_id}", requireUser(http.HandlerFunc(s.handleDeleteReferenceFaceByID)))
	mux.HandleFunc("GET /signaling", s.handleSignaling)
	mux.Handle("/client/", s.clientHandler())
	// pprof는 힙·고루틴 덤프와 프로세스 argv를 그대로 노출하므로 기본값은 꺼짐이다.
	// 인증 래퍼를 씌우지 않고 등록 자체를 막는다 — 라우트가 없으면 404로 떨어진다.
	// 프로파일링이 필요할 때만 PPROF_ENABLED=true로 켠다.
	if cfg.PprofEnabled {
		mux.Handle("/debug/pprof/", http.DefaultServeMux)
	}
	// 세션이 어떤 이유로 끝나든(WebRTC 실패·유예 시간 초과·로그아웃·명시적
	// 삭제) 라이브까지 가지 못한 방송은 채널에 남으므로 여기서 치운다.
	// 세션 매니저 없이 조립되는 경로(일부 테스트)도 있어 nil을 확인한다.
	if sessions != nil {
		sessions.SetBroadcastCleanup(s.discardBroadcast)
	}
	s.handler = recoverMiddleware(logger, corsMiddleware(origins, requestIDMiddleware(mux)))
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"service": "inno-live-server", "status": "running"})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	runtimes := make([]map[string]any, 0, 1)
	if s.ai != nil {
		for _, client := range s.ai.Clients() {
			runtimes = append(runtimes, map[string]any{
				"kind":    "external_grpc",
				"address": client.Address(),
				"mode":    s.cfg.PrivacyMode,
				"state":   client.State(),
				"ready":   client.Ready(),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":     "inno-live-server",
		"status":      "ready",
		"ai_runtimes": runtimes,
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.WritePrometheus(w)
}

func (s *Server) handleWebRTCConfig(w http.ResponseWriter, _ *http.Request) {
	type iceServer struct {
		URLs       []string `json:"urls"`
		Username   string   `json:"username,omitempty"`
		Credential any      `json:"credential,omitempty"`
	}
	items := make([]iceServer, 0)
	for _, server := range s.sessions.ICEServers() {
		items = append(items, iceServer{URLs: server.URLs, Username: server.Username, Credential: server.Credential})
	}
	writeJSON(w, http.StatusOK, map[string]any{"iceServers": items})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	request := struct {
		Metadata map[string]string `json:"metadata"`
	}{Metadata: map[string]string{}}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := decodeOptionalJSON(r.Body, &request); err != nil {
		writeError(w, badRequest("Invalid session request.", map[string]any{"error": err.Error()}))
		return
	}
	userID, _ := auth.UserIDFromContext(r.Context())
	liveSession, ownerToken, err := s.sessions.CreateForUser(userID, request.Metadata)
	if errors.Is(err, session.ErrCapacityExceeded) {
		active, limit := s.sessions.Capacity()
		s.logger.Info("session rejected at capacity", "active_sessions", active, "max_sessions", limit)
		writeError(w, apiError{
			Status:  http.StatusServiceUnavailable,
			Code:    "capacity_exceeded",
			Message: "The server has reached its maximum number of concurrent sessions.",
			Details: map[string]any{"active_sessions": active, "max_sessions": limit},
		})
		return
	}
	if err != nil {
		s.logger.Error("create session failed", "error", err)
		writeError(w, internalError())
		return
	}
	// owner_token is returned exactly once, here, and never re-exposed.
	writeJSON(w, http.StatusCreated, struct {
		session.Response
		OwnerToken string `json:"owner_token"`
	}{Response: liveSession.Response(), OwnerToken: ownerToken})
}

func (s *Server) handleGetSession(w http.ResponseWriter, _ *http.Request, liveSession *session.Session) {
	writeJSON(w, http.StatusOK, liveSession.Response())
}

// 준비된 방송 정리는 세션 삭제 훅(SetBroadcastCleanup)이 맡는다 — 명시적
// 삭제뿐 아니라 WebRTC 실패·유예 시간 초과·로그아웃도 같은 경로로 모인다.
func (s *Server) handleDeleteSession(w http.ResponseWriter, _ *http.Request, liveSession *session.Session) {
	if err := s.sessions.Delete(liveSession.ID, "delete_session"); err != nil {
		writeSessionError(w, err, liveSession.ID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePrepareStream은 송출 준비다(#142): 요청 사용자의 연결된 플랫폼 계정으로
// 방송을 만들고(시청자에게 노출되지 않는 상태) egress를 붙인다. 시청자에게
// 공개되는 전환은 golive가 따로 한다.
//
// 준비 옵션은 저장된 방송 설정(PUT /sessions/{id}/broadcast)이 단일 출처다 —
// 요청 바디로 덮어쓰는 경로를 두면 설정 화면과 실제 방송이 어긋난다.
func (s *Server) handlePrepareStream(w http.ResponseWriter, r *http.Request, liveSession *session.Session) {
	request := struct {
		Provider string `json:"provider"`
	}{}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := decodeOptionalJSON(r.Body, &request); err != nil {
		writeError(w, badRequest("Invalid stream prepare request.", map[string]any{"error": err.Error()}))
		return
	}
	providerName := auth.StreamingProvider(strings.TrimSpace(request.Provider))
	if providerName == "" {
		providerName = auth.StreamingProviderYouTube
	}
	provider := s.streaming[providerName]
	if provider == nil {
		// 플랫폼 송출이 조립되지 않은 배포(자격증명 미설정·벤치)에서는 종전
		// 계약(501)을 유지한다.
		writeError(w, apiError{Status: http.StatusNotImplemented, Code: "not_supported", Message: "Streaming to this platform is not configured on the server.", Details: map[string]any{"provider": providerName}})
		return
	}
	// 플랫폼을 부르기 전에 준비 구간을 선점한다. 방송을 만든 뒤 거절하면
	// 채널에 빈 방송이 남고, 선점하지 않으면 플랫폼 왕복 중에 들어온
	// PUT /broadcast가 통과해 저장값과 실제 방송이 갈린다.
	if _, err := s.sessions.BeginBroadcastPrepare(liveSession.ID); err != nil {
		writeBroadcastBeginError(w, err, liveSession.ID)
		return
	}
	// 설정은 선점 이후에 읽는다 — 이 시점부터 저장값은 바뀌지 않는다.
	options := prepareOptionsFrom(liveSession.BroadcastSettings())
	if options.MadeForKids == nil {
		// 시청자층 신고는 플랫폼이 법적으로 요구하는 사용자 선택 항목이라
		// 서버가 기본값으로 대신 신고하지 않는다.
		s.sessions.ResetBroadcastPreparation(liveSession.ID)
		writeError(w, badRequest("made_for_kids must be specified in the broadcast settings.", map[string]any{"field": "made_for_kids"}))
		return
	}
	prepared, err := provider.Prepare(r.Context(), liveSession.UserID, options)
	preparedRecord := session.PlatformBroadcast{
		Provider:    string(prepared.Provider),
		BroadcastID: prepared.BroadcastID,
		StreamID:    prepared.StreamID,
	}
	if err != nil {
		s.sessions.ResetBroadcastPreparation(liveSession.ID)
		switch {
		case errors.Is(err, auth.ErrStreamingNotConnected):
			writeError(w, apiError{Status: http.StatusConflict, Code: "streaming_not_connected", Message: "Connect a streaming account before starting a stream.", Details: map[string]any{"provider": providerName}})
		case errors.Is(err, auth.ErrStreamingReconnectRequired):
			// 재시도로 복구되지 않는 상태 — "잠시 후 재시도"가 아니라
			// "재연결"을 안내해야 하므로 일반 준비 실패(502)와 구분한다.
			writeError(w, apiError{Status: http.StatusConflict, Code: "streaming_reconnect_required", Message: "The streaming account needs to be reconnected.", Details: map[string]any{"provider": providerName}})
		case errors.Is(err, streaming.ErrLiveStreamingBlocked):
			writeError(w, apiError{Status: http.StatusForbidden, Code: "live_streaming_blocked", Message: "The channel is not enabled for live streaming. Enabling can take up to 24 hours.", Details: map[string]any{"help_url": streaming.LiveStreamingHelpURL}})
		default:
			s.logger.Error("prepare platform broadcast failed", "session_id", liveSession.ID, "provider", providerName, "error", err)
			writeError(w, apiError{Status: http.StatusBadGateway, Code: "streaming_prepare_failed", Message: "The streaming platform could not prepare the broadcast."})
		}
		return
	}
	if _, err := s.sessions.StartStream(liveSession.ID, prepared.IngestURL); err != nil {
		// egress를 못 붙이면 방금 만든 방송은 쓸 데가 없으므로 되돌린다.
		s.discardPreparedBroadcast(liveSession, preparedRecord)
		s.sessions.ResetBroadcastPreparation(liveSession.ID)
		switch {
		case errors.Is(err, session.ErrNoVideoTrack):
			writeError(w, apiError{Status: http.StatusConflict, Code: "conflict", Message: "Cannot start stream before a video track is available.", Details: map[string]any{"session_id": liveSession.ID}})
		case errors.Is(err, session.ErrStreamActive):
			writeError(w, apiError{Status: http.StatusConflict, Code: "stream_already_active", Message: "The stream is already active.", Details: map[string]any{"session_id": liveSession.ID}})
		default:
			writeSessionError(w, err, liveSession.ID)
		}
		return
	}
	if _, err := s.sessions.MarkBroadcastPrepared(liveSession.ID, preparedRecord); err != nil {
		// 세션이 방금 닫힌 경우 등 — 기록하지 못한 방송은 남겨두지 않는다.
		s.discardPreparedBroadcast(liveSession, preparedRecord)
		s.sessions.ResetBroadcastPreparation(liveSession.ID)
		s.logger.Error("record prepared broadcast failed", "session_id", liveSession.ID, "error", err)
		writeSessionError(w, err, liveSession.ID)
		return
	}
	// 카테고리·썸네일 반영 실패는 방송을 막지 않고 경고로만 알린다.
	writeJSON(w, http.StatusOK, struct {
		session.Response
		Warnings []streaming.Warning `json:"warnings,omitempty"`
	}{Response: liveSession.Response(), Warnings: prepared.Warnings})
}

// handleGoLive는 준비된 방송을 시청자에게 공개되는 라이브로 전환한다.
func (s *Server) handleGoLive(w http.ResponseWriter, r *http.Request, liveSession *session.Session) {
	broadcast, phase := liveSession.PlatformBroadcast()
	if phase != session.BroadcastPhasePrepared {
		writeBroadcastPhaseError(w, phase, liveSession.ID)
		return
	}
	providerName := auth.StreamingProvider(broadcast.Provider)
	provider := s.streaming[providerName]
	if provider == nil {
		writeError(w, apiError{Status: http.StatusNotImplemented, Code: "not_supported", Message: "Streaming to this platform is not configured on the server.", Details: map[string]any{"provider": providerName}})
		return
	}
	err := provider.GoLive(r.Context(), liveSession.UserID, streaming.PreparedBroadcast{
		Provider:    providerName,
		BroadcastID: broadcast.BroadcastID,
		StreamID:    broadcast.StreamID,
	})
	switch {
	case err == nil:
	case errors.Is(err, streaming.ErrBroadcastNotReady):
		// 송출 프레임이 플랫폼에 아직 도착하지 않은 상태 — 잠시 후 재시도로
		// 풀리므로 준비 실패(502)와 구분한다.
		writeError(w, apiError{Status: http.StatusConflict, Code: "broadcast_not_ready", Message: "The broadcast is not ready to go live yet. Retry once the stream is being received.", Details: map[string]any{"session_id": liveSession.ID}})
		return
	case errors.Is(err, auth.ErrStreamingReconnectRequired):
		writeError(w, apiError{Status: http.StatusConflict, Code: "streaming_reconnect_required", Message: "The streaming account needs to be reconnected.", Details: map[string]any{"provider": providerName}})
		return
	default:
		s.logger.Error("go live failed", "session_id", liveSession.ID, "provider", providerName, "error", err)
		writeError(w, apiError{Status: http.StatusBadGateway, Code: "streaming_golive_failed", Message: "The streaming platform could not switch the broadcast to live."})
		return
	}
	if _, err := s.sessions.MarkBroadcastLive(liveSession.ID); err != nil {
		s.logger.Error("record live broadcast failed", "session_id", liveSession.ID, "error", err)
		writeSessionError(w, err, liveSession.ID)
		return
	}
	writeJSON(w, http.StatusOK, liveSession.Response().Stream)
}

// discardPreparedBroadcast는 준비까지 끝냈지만 쓰이지 못한 방송을 플랫폼에서
// 정리한다. autoStart를 끈 뒤로는 이런 방송을 아무도 치워주지 않는다.
// 실패해도 요청 처리에는 영향이 없으므로 로그만 남긴다.
func (s *Server) discardPreparedBroadcast(liveSession *session.Session, broadcast session.PlatformBroadcast) {
	s.discardBroadcast(liveSession.UserID, broadcast)
}

// discardBroadcast는 세션 객체 없이도 방송을 치운다 — 세션 종료 훅은 이미
// 사라진 세션에 대해 호출되기 때문이다.
func (s *Server) discardBroadcast(userID uuid.UUID, broadcast session.PlatformBroadcast) {
	providerName := auth.StreamingProvider(broadcast.Provider)
	provider := s.streaming[providerName]
	if provider == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), platformCleanupTimeout)
	defer cancel()
	err := provider.Stop(ctx, userID, streaming.PreparedBroadcast{
		Provider:    providerName,
		BroadcastID: broadcast.BroadcastID,
		StreamID:    broadcast.StreamID,
	})
	if err != nil {
		s.logger.Warn("discard prepared broadcast failed", "user_id", userID, "provider", providerName, "broadcast_id", broadcast.BroadcastID, "error", err)
	}
}

// writeBroadcastBeginError는 준비 선점 실패를 응답으로 옮긴다. 세션이 사라진
// 경우와 단계 위반을 구분한다.
func writeBroadcastBeginError(w http.ResponseWriter, err error, sessionID string) {
	switch {
	case errors.Is(err, session.ErrBroadcastLive):
		writeBroadcastPhaseError(w, session.BroadcastPhaseLive, sessionID)
	case errors.Is(err, session.ErrBroadcastPrepared):
		writeBroadcastPhaseError(w, session.BroadcastPhasePrepared, sessionID)
	default:
		writeSessionError(w, err, sessionID)
	}
}

// writeBroadcastPhaseError는 방송 단계 위반을 409로 옮긴다.
func writeBroadcastPhaseError(w http.ResponseWriter, phase session.BroadcastPhase, sessionID string) {
	switch phase {
	case session.BroadcastPhaseLive:
		writeError(w, apiError{Status: http.StatusConflict, Code: "broadcast_live", Message: "The broadcast is already live.", Details: map[string]any{"session_id": sessionID}})
	case session.BroadcastPhasePreparing, session.BroadcastPhasePrepared:
		writeError(w, apiError{Status: http.StatusConflict, Code: "broadcast_prepared", Message: "The broadcast is already prepared.", Details: map[string]any{"session_id": sessionID}})
	default:
		writeError(w, apiError{Status: http.StatusConflict, Code: "broadcast_not_prepared", Message: "Prepare the broadcast before going live.", Details: map[string]any{"session_id": sessionID}})
	}
}

// prepareOptionsFrom은 저장된 방송 설정을 플랫폼 준비 옵션으로 옮긴다.
func prepareOptionsFrom(settings session.YouTubeBroadcastSettings) streaming.PrepareOptions {
	options := streaming.PrepareOptions{
		Title:       settings.Title,
		Description: settings.Description,
		Privacy:     settings.Privacy,
		MadeForKids: settings.MadeForKids,
		CategoryID:  settings.CategoryID,
	}
	if settings.Thumbnail != nil {
		options.Thumbnail = &streaming.Thumbnail{MIME: settings.Thumbnail.MIME, Data: settings.Thumbnail.Data}
	}
	return options
}

// handlePutBroadcast는 방송 설정을 저장·검증만 한다. 플랫폼 호출은 송출
// 시작(prepare) 시점으로 미룬다 — 설정 도중 방송이 만들어지지 않게 하려는 것.
// PUT이므로 생략한 필드는 비워지는 전체 교체다.
func (s *Server) handlePutBroadcast(w http.ResponseWriter, r *http.Request, liveSession *session.Session) {
	request := struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Privacy     string `json:"privacy"`
		MadeForKids *bool  `json:"made_for_kids"`
		CategoryID  string `json:"category_id"`
		Thumbnail   *struct {
			MIME       string `json:"mime"`
			DataBase64 string `json:"data_base64"`
		} `json:"thumbnail"`
	}{}
	r.Body = http.MaxBytesReader(w, r.Body, maxBroadcastBody)
	if err := decodeOptionalJSON(r.Body, &request); err != nil {
		writeError(w, badRequest("Invalid broadcast settings request.", map[string]any{"error": err.Error()}))
		return
	}
	settings := session.YouTubeBroadcastSettings{
		Title:       strings.TrimSpace(request.Title),
		Description: request.Description,
		Privacy:     strings.TrimSpace(request.Privacy),
		MadeForKids: request.MadeForKids,
		CategoryID:  strings.TrimSpace(request.CategoryID),
	}
	if request.Thumbnail != nil {
		data, err := base64.StdEncoding.DecodeString(request.Thumbnail.DataBase64)
		if err != nil {
			writeError(w, badRequest("Invalid broadcast settings request.", map[string]any{"field": "thumbnail.data_base64", "error": err.Error()}))
			return
		}
		settings.Thumbnail = &session.YouTubeThumbnail{MIME: strings.TrimSpace(request.Thumbnail.MIME), Data: data}
	}
	if _, err := s.sessions.SetBroadcastSettings(liveSession.ID, settings); err != nil {
		var invalid session.InvalidBroadcastSettingsError
		switch {
		case errors.As(err, &invalid):
			writeError(w, badRequest("Invalid broadcast settings request.", map[string]any{"field": invalid.Field, "reason": invalid.Reason}))
		case errors.Is(err, session.ErrBroadcastPrepared):
			writeBroadcastPhaseError(w, session.BroadcastPhasePrepared, liveSession.ID)
		case errors.Is(err, session.ErrBroadcastLive):
			writeBroadcastPhaseError(w, session.BroadcastPhaseLive, liveSession.ID)
		default:
			writeSessionError(w, err, liveSession.ID)
		}
		return
	}
	writeJSON(w, http.StatusOK, liveSession.Response())
}

// handleStopStream은 egress만 종료한다(뷰어 송출·세션은 유지). 라이브였던
// 방송의 종료는 autoStop이 담당하므로(송출 중단 약 1분 후 반영) 플랫폼 API
// 호출이 없다. 다만 라이브 전까지 못 간 방송은 autoStop이 손대지 않으므로
// 여기서 지운다(#142).
func (s *Server) handleStopStream(w http.ResponseWriter, _ *http.Request, liveSession *session.Session) {
	broadcast, phase := liveSession.PlatformBroadcast()
	if _, err := s.sessions.StopStream(liveSession.ID); err != nil {
		if errors.Is(err, session.ErrStreamNotActive) {
			writeError(w, apiError{Status: http.StatusConflict, Code: "stream_not_active", Message: "The stream is not active.", Details: map[string]any{"session_id": liveSession.ID}})
			return
		}
		writeSessionError(w, err, liveSession.ID)
		return
	}
	if phase == session.BroadcastPhasePrepared {
		s.discardPreparedBroadcast(liveSession, broadcast)
	}
	writeJSON(w, http.StatusOK, liveSession.Response().Stream)
}

// handlePauseStream은 RTMP·플랫폼 방송을 유지하고 소스 일시 중단을 기록한다.
// 미디어 egress는 이 상태를 보고 취소 슬레이트로 전환하며, 최종 송출 중지와는
// 의도적으로 다른 동작이다.
func (s *Server) handlePauseStream(w http.ResponseWriter, _ *http.Request, liveSession *session.Session) {
	if _, err := s.sessions.PauseStream(liveSession.ID); err != nil {
		switch {
		case errors.Is(err, session.ErrStreamNotActive):
			writeError(w, apiError{Status: http.StatusConflict, Code: "stream_not_active", Message: "The stream is not active.", Details: map[string]any{"session_id": liveSession.ID}})
		case errors.Is(err, session.ErrStreamPaused):
			writeError(w, apiError{Status: http.StatusConflict, Code: "stream_already_paused", Message: "The stream is already paused.", Details: map[string]any{"session_id": liveSession.ID}})
		default:
			writeSessionError(w, err, liveSession.ID)
		}
		return
	}
	writeJSON(w, http.StatusOK, liveSession.Response().Stream)
}

func (s *Server) handleResumeStream(w http.ResponseWriter, _ *http.Request, liveSession *session.Session) {
	if _, err := s.sessions.ResumeStream(liveSession.ID); err != nil {
		switch {
		case errors.Is(err, session.ErrStreamNotActive):
			writeError(w, apiError{Status: http.StatusConflict, Code: "stream_not_active", Message: "The stream is not active.", Details: map[string]any{"session_id": liveSession.ID}})
		case errors.Is(err, session.ErrStreamNotPaused):
			writeError(w, apiError{Status: http.StatusConflict, Code: "stream_not_paused", Message: "The stream is not paused.", Details: map[string]any{"session_id": liveSession.ID}})
		default:
			writeSessionError(w, err, liveSession.ID)
		}
		return
	}
	writeJSON(w, http.StatusOK, liveSession.Response().Stream)
}

// handlePatchAnonymization controls AI privacy processing without changing the
// WebRTC session or an active RTMP/YouTube broadcast.
func (s *Server) handlePatchAnonymization(w http.ResponseWriter, r *http.Request, liveSession *session.Session) {
	request := struct {
		Enabled *bool `json:"enabled"`
	}{}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := decodeOptionalJSON(r.Body, &request); err != nil || request.Enabled == nil {
		if err == nil {
			err = errors.New("enabled is required")
		}
		writeError(w, badRequest("Invalid anonymization request.", map[string]any{"error": err.Error()}))
		return
	}
	if _, err := s.sessions.SetAnonymizationEnabled(liveSession.ID, *request.Enabled); err != nil {
		writeSessionError(w, err, liveSession.ID)
		return
	}
	writeJSON(w, http.StatusOK, liveSession.Response())
}

func decodeOptionalJSON(body io.Reader, target any) error {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func recoverMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("HTTP handler panicked", "method", r.Method, "path", r.URL.Path, "panic", recovered)
				writeError(w, internalError())
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// reqWriter carries the per-request id so writeError can echo it in the body
// without threading it through every handler signature.
type reqWriter struct {
	http.ResponseWriter
	requestID string
}

// Hijack forwards to the underlying writer so WebSocket upgrades (the /signaling
// endpoint) keep working — gorilla/websocket requires http.Hijacker, which the
// wrapper would otherwise hide.
func (w *reqWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hj.Hijack()
}

// Flush forwards to the underlying writer when it supports streaming responses.
func (w *reqWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// requestIDMiddleware propagates an X-Request-ID (client-supplied or generated),
// sets it on the response header, and wraps the writer so errors include it.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(&reqWriter{ResponseWriter: w, requestID: id}, r)
	})
}

func corsMiddleware(origins origin.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestOrigin := strings.TrimSpace(r.Header.Get("Origin"))
		if requestOrigin != "" {
			allowedOrigin, ok := origins.AllowedOrigin(requestOrigin)
			if !ok {
				writeError(w, apiError{Status: http.StatusForbidden, Code: "origin_not_allowed", Message: "Origin is not allowed."})
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")
			if allowedOrigin != "*" {
				w.Header().Add("Vary", "Origin")
			}
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			requestedHeaders := r.Header.Get("Access-Control-Request-Headers")
			if strings.TrimSpace(requestedHeaders) == "" {
				requestedHeaders = "Content-Type, X-Client-ID, X-Request-ID"
			}
			w.Header().Set("Access-Control-Allow-Headers", requestedHeaders)
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type apiError struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func badRequest(message string, details map[string]any) apiError {
	return apiError{Status: http.StatusBadRequest, Code: "bad_request", Message: message, Details: details}
}

func internalError() apiError {
	return apiError{Status: http.StatusInternalServerError, Code: "internal_error", Message: "An unexpected server error occurred."}
}

func writeSessionError(w http.ResponseWriter, err error, id string) {
	if errors.Is(err, session.ErrNotFound) {
		writeError(w, apiError{Status: http.StatusNotFound, Code: "not_found", Message: "Session not found.", Details: map[string]any{"session_id": id}})
		return
	}
	writeError(w, internalError())
}

func writeError(w http.ResponseWriter, err apiError) {
	payload := map[string]any{"code": err.Code, "message": err.Message}
	if len(err.Details) > 0 {
		payload["details"] = err.Details
	}
	body := map[string]any{"error": payload}
	if rw, ok := w.(*reqWriter); ok && rw.requestID != "" {
		body["request_id"] = rw.requestID
	}
	writeJSON(w, err.Status, body)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		fmt.Fprintln(w, `{}`)
	}
}
