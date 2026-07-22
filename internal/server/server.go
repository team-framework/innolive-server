package server

import (
	"bufio"
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

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/session"
)

const maxJSONBody = 1 << 20

type Server struct {
	cfg      config.Config
	logger   *slog.Logger
	metrics  *metrics.Registry
	sessions *session.Manager
	handler  http.Handler
}

func New(cfg config.Config, logger *slog.Logger, registry *metrics.Registry, sessions *session.Manager) *Server {
	s := &Server{
		cfg:      cfg,
		logger:   logger,
		metrics:  registry,
		sessions: sessions,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /webrtc/config", s.handleWebRTCConfig)
	mux.HandleFunc("POST /sessions", s.handleCreateSession)
	mux.HandleFunc("GET /sessions", s.handleListSessions)
	mux.HandleFunc("GET /sessions/{session_id}", s.handleGetSession)
	mux.HandleFunc("DELETE /sessions/{session_id}", s.handleDeleteSession)
	mux.HandleFunc("POST /sessions/{session_id}/stream/start", s.handleStartStream)
	mux.HandleFunc("POST /sessions/{session_id}/stream/stop", s.handleStopStream)
	mux.HandleFunc("GET /signaling", s.handleSignaling)
	mux.Handle("/debug/pprof/", http.DefaultServeMux)
	s.handler = recoverMiddleware(logger, corsMiddleware(requestIDMiddleware(mux)))
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"service": "inno-live-server", "status": "running"})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	runtimes := make([]map[string]any, 0, 1)
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
	liveSession, err := s.sessions.Create(request.Metadata)
	if err != nil {
		s.logger.Error("create session failed", "error", err)
		writeError(w, internalError())
		return
	}
	writeJSON(w, http.StatusCreated, liveSession.Response())
}

func (s *Server) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	items := s.sessions.List()
	responses := make([]session.Response, 0, len(items))
	for _, item := range items {
		responses = append(responses, item.Response())
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": responses})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	liveSession, err := s.sessions.Get(r.PathValue("session_id"))
	if err != nil {
		writeSessionError(w, err, r.PathValue("session_id"))
		return
	}
	writeJSON(w, http.StatusOK, liveSession.Response())
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("session_id")
	if err := s.sessions.Delete(id, "delete_session"); err != nil {
		writeSessionError(w, err, id)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStartStream(w http.ResponseWriter, r *http.Request) {
	liveSession, err := s.sessions.Get(r.PathValue("session_id"))
	if err != nil {
		writeSessionError(w, err, r.PathValue("session_id"))
		return
	}
	response := liveSession.Response()
	if response.Media.RawVideoTrack == nil {
		writeError(w, apiError{Status: http.StatusConflict, Code: "conflict", Message: "Cannot start stream before a video track is available.", Details: map[string]any{"session_id": liveSession.ID}})
		return
	}
	writeError(w, apiError{Status: http.StatusNotImplemented, Code: "not_supported", Message: "RTMP publishing is not part of the Go comparison server."})
}

func (s *Server) handleStopStream(w http.ResponseWriter, r *http.Request) {
	liveSession, err := s.sessions.Get(r.PathValue("session_id"))
	if err != nil {
		writeSessionError(w, err, r.PathValue("session_id"))
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

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")
			w.Header().Add("Vary", "Origin")
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
