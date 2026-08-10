package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"strings"

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
	mux.Handle("POST /sessions/{session_id}/stream/start", requireUser(s.requireSessionOwner(s.handleStartStream)))
	mux.Handle("POST /sessions/{session_id}/stream/stop", requireUser(s.requireSessionOwner(s.handleStopStream)))
	mux.Handle("GET /reference-face", requireUser(http.HandlerFunc(s.handleGetReferenceFace)))
	mux.Handle("POST /reference-face", requireUser(http.HandlerFunc(s.handlePostReferenceFace)))
	mux.Handle("DELETE /reference-face", requireUser(http.HandlerFunc(s.handleDeleteReferenceFace)))
	mux.Handle("DELETE /reference-face/{face_id}", requireUser(http.HandlerFunc(s.handleDeleteReferenceFaceByID)))
	mux.HandleFunc("GET /signaling", s.handleSignaling)
	mux.Handle("/client/", s.clientHandler())
	mux.Handle("/debug/pprof/", http.DefaultServeMux)
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

func (s *Server) handleDeleteSession(w http.ResponseWriter, _ *http.Request, liveSession *session.Session) {
	if err := s.sessions.Delete(liveSession.ID, "delete_session"); err != nil {
		writeSessionError(w, err, liveSession.ID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleStartStream은 명시적 송출 시작(#83)이다: 요청 사용자의 연결된 플랫폼
// 계정으로 방송을 준비(Prepare)하고, 세션의 처리 출력에 egress를 붙인다.
func (s *Server) handleStartStream(w http.ResponseWriter, r *http.Request, liveSession *session.Session) {
	request := struct {
		Provider string `json:"provider"`
		Title    string `json:"title"`
		Privacy  string `json:"privacy"`
	}{}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := decodeOptionalJSON(r.Body, &request); err != nil {
		writeError(w, badRequest("Invalid stream start request.", map[string]any{"error": err.Error()}))
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
	prepared, err := provider.Prepare(r.Context(), liveSession.UserID, streaming.PrepareOptions{
		Title:   request.Title,
		Privacy: request.Privacy,
	})
	if err != nil {
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
	writeJSON(w, http.StatusOK, liveSession.Response())
}

// handleStopStream은 egress만 종료한다(뷰어 송출·세션은 유지). 플랫폼 쪽
// 방송 종료는 autoStop이 담당하므로(송출 중단 약 1분 후 반영) 플랫폼 API
// 호출이 없다.
func (s *Server) handleStopStream(w http.ResponseWriter, _ *http.Request, liveSession *session.Session) {
	if _, err := s.sessions.StopStream(liveSession.ID); err != nil {
		if errors.Is(err, session.ErrStreamNotActive) {
			writeError(w, apiError{Status: http.StatusConflict, Code: "stream_not_active", Message: "The stream is not active.", Details: map[string]any{"session_id": liveSession.ID}})
			return
		}
		writeSessionError(w, err, liveSession.ID)
		return
	}
	writeJSON(w, http.StatusOK, liveSession.Response().Stream)
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
