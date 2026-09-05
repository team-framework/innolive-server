package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"inno-live-server/internal/session"
)

var guestSSEStatusInterval = 15 * time.Second

func (s *Server) guestID(w http.ResponseWriter, r *http.Request, create bool) (string, bool) {
	if cookie, err := r.Cookie(guestCookieName); err == nil && strings.TrimSpace(cookie.Value) != "" {
		return cookie.Value, true
	}
	if !create {
		return "", false
	}
	value, err := newGuestSecret()
	if err != nil {
		writeError(w, internalError())
		return "", false
	}
	http.SetCookie(w, &http.Cookie{Name: guestCookieName, Value: value, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteNoneMode, MaxAge: int((24 * time.Hour).Seconds())})
	return value, true
}

func (s *Server) guestQueueError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrGuestQueueUnavailable):
		writeError(w, apiError{Status: http.StatusServiceUnavailable, Code: "queue_unavailable", Message: "Guest queue is temporarily unavailable."})
	case errors.Is(err, ErrGuestTicketNotFound):
		writeError(w, apiError{Status: http.StatusNotFound, Code: "not_found", Message: "Guest queue ticket not found."})
	case errors.Is(err, ErrGuestForbidden):
		writeError(w, apiError{Status: http.StatusForbidden, Code: "forbidden", Message: "Guest queue ticket does not belong to this browser."})
	case errors.Is(err, ErrGuestAdmissionInvalid):
		writeError(w, apiError{Status: http.StatusConflict, Code: "admission_invalid", Message: "Guest admission is invalid or expired."})
	case errors.Is(err, ErrGuestQueueFull):
		w.Header().Set("Retry-After", "60")
		writeError(w, apiError{Status: http.StatusTooManyRequests, Code: "queue_full", Message: "Guest queue is full."})
	case errors.Is(err, ErrGuestRateLimited):
		w.Header().Set("Retry-After", "60")
		writeError(w, apiError{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "Too many guest queue requests."})
	default:
		writeError(w, internalError())
	}
}

func (s *Server) handleGuestQueueCreate(w http.ResponseWriter, r *http.Request) {
	guest, ok := s.guestID(w, r, true)
	if !ok {
		return
	}
	ticket, err := s.guestQueue.CreateOrGet(r.Context(), guest, r.RemoteAddr, r.Header.Get("X-Forwarded-For"))
	if err != nil {
		s.guestQueueError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ticket)
}

func (s *Server) guestTicket(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	guest, ok := s.guestID(w, r, false)
	if !ok {
		writeError(w, apiError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "Guest cookie is required."})
		return "", "", false
	}
	return guest, r.PathValue("ticket_id"), true
}

func (s *Server) handleGuestQueueStatus(w http.ResponseWriter, r *http.Request) {
	guest, id, ok := s.guestTicket(w, r)
	if !ok {
		return
	}
	ticket, err := s.guestQueue.Status(r.Context(), guest, id)
	if err != nil {
		s.guestQueueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ticket)
}
func (s *Server) handleGuestQueueHeartbeat(w http.ResponseWriter, r *http.Request) {
	guest, id, ok := s.guestTicket(w, r)
	if !ok {
		return
	}
	ticket, err := s.guestQueue.Heartbeat(r.Context(), guest, id)
	if err != nil {
		s.guestQueueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ticket)
}
func (s *Server) handleGuestQueueCancel(w http.ResponseWriter, r *http.Request) {
	guest, id, ok := s.guestTicket(w, r)
	if !ok {
		return
	}
	if err := s.guestQueue.Cancel(r.Context(), guest, id); err != nil {
		s.guestQueueError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGuestQueueEvents(w http.ResponseWriter, r *http.Request) {
	guest, id, ok := s.guestTicket(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, internalError())
		return
	}
	pubsub := s.guestQueue.Subscribe(r.Context())
	defer pubsub.Close()
	channel := pubsub.Channel()
	send := func() bool {
		ticket, err := s.guestQueue.Status(r.Context(), guest, id)
		if err != nil {
			event, code := "error", "queue_unavailable"
			if errors.Is(err, ErrGuestTicketNotFound) {
				event, code = "expired", "not_found"
			}
			if errors.Is(err, ErrGuestForbidden) {
				code = "forbidden"
			}
			fmt.Fprintf(w, "event: %s\ndata: {\"code\":%q}\n\n", event, code)
			flusher.Flush()
			return false
		}
		data, _ := json.Marshal(ticket)
		event := "queue"
		if ticket.Status == "admitted" {
			event = "admitted"
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}
	heartbeat := time.NewTicker(guestSSEStatusInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if !send() {
				return
			}
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case <-channel:
			if !send() {
				return
			}
		}
	}
}

func (s *Server) handleGuestSessionCreate(w http.ResponseWriter, r *http.Request) {
	guest, ok := s.guestID(w, r, false)
	if !ok {
		writeError(w, apiError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "Guest cookie is required."})
		return
	}
	request := struct {
		AdmissionToken string            `json:"admission_token"`
		Metadata       map[string]string `json:"metadata"`
	}{}
	if err := decodeOptionalJSON(http.MaxBytesReader(w, r.Body, maxJSONBody), &request); err != nil {
		writeError(w, badRequest("Invalid guest session request.", nil))
		return
	}
	live, owner, err := s.guestQueue.Consume(r.Context(), guest, strings.TrimSpace(request.AdmissionToken), request.Metadata)
	if err != nil {
		s.guestQueueError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		session.Response
		OwnerToken string `json:"owner_token"`
		ICEServers any    `json:"iceServers"`
	}{Response: live.Response(), OwnerToken: owner, ICEServers: s.sessions.ICEServers()})
}

func (s *Server) handleGuestGetSession(w http.ResponseWriter, r *http.Request) {
	live, _, ok := s.guestSessionRequest(w, r)
	if ok {
		writeJSON(w, http.StatusOK, live.Response())
	}
}

func (s *Server) handleGuestDeleteSession(w http.ResponseWriter, r *http.Request) {
	live, _, ok := s.guestSessionRequest(w, r)
	if !ok {
		return
	}
	if err := s.sessions.Delete(live.ID, "delete_guest_session"); err != nil {
		writeSessionError(w, err, live.ID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGuestPatchAnonymization(w http.ResponseWriter, r *http.Request) {
	live, _, ok := s.guestSessionRequest(w, r)
	if !ok {
		return
	}
	request := struct {
		Enabled *bool `json:"enabled"`
	}{}
	if err := decodeOptionalJSON(http.MaxBytesReader(w, r.Body, maxJSONBody), &request); err != nil || request.Enabled == nil {
		writeError(w, badRequest("Invalid anonymization request.", nil))
		return
	}
	if _, err := s.sessions.SetAnonymizationEnabled(live.ID, *request.Enabled); err != nil {
		writeSessionError(w, err, live.ID)
		return
	}
	writeJSON(w, http.StatusOK, live.Response())
}

func (s *Server) guestSessionRequest(w http.ResponseWriter, r *http.Request) (*session.Session, *http.Request, bool) {
	guest, ok := s.guestID(w, r, false)
	if !ok {
		writeError(w, apiError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "Guest cookie is required."})
		return nil, nil, false
	}
	live, err := s.sessions.VerifyOwner(r.PathValue("session_id"), strings.TrimSpace(r.Header.Get("X-Session-Owner-Token")))
	if errors.Is(err, session.ErrNotFound) {
		writeError(w, apiError{Status: http.StatusNotFound, Code: "not_found", Message: "Session not found."})
		return nil, nil, false
	}
	if err != nil || live.GuestID == "" || live.GuestID != guestHash(guest) {
		writeError(w, apiError{Status: http.StatusForbidden, Code: "forbidden", Message: "Guest session does not belong to this browser."})
		return nil, nil, false
	}
	return live, r.WithContext(context.WithValue(r.Context(), guestReferenceContextKey{}, live.AIClientID)), true
}

func (s *Server) handleGuestGetReferenceFace(w http.ResponseWriter, r *http.Request) {
	_, request, ok := s.guestSessionRequest(w, r)
	if ok {
		s.handleGetReferenceFace(w, request)
	}
}
func (s *Server) handleGuestPostReferenceFace(w http.ResponseWriter, r *http.Request) {
	s.withGuestReferenceSession(w, r, s.handlePostReferenceFace)
}
func (s *Server) handleGuestDeleteReferenceFace(w http.ResponseWriter, r *http.Request) {
	s.withGuestReferenceSession(w, r, s.handleDeleteReferenceFace)
}

func (s *Server) withGuestReferenceSession(w http.ResponseWriter, r *http.Request, next func(http.ResponseWriter, *http.Request)) {
	unlock, closed := s.guestReference.Lock(r.PathValue("session_id"))
	defer unlock()
	if closed {
		writeError(w, apiError{Status: http.StatusNotFound, Code: "not_found", Message: "Session not found."})
		return
	}
	_, request, ok := s.guestSessionRequest(w, r)
	if !ok {
		return
	}
	request = request.WithContext(context.WithValue(request.Context(), guestReferenceGateContextKey{}, struct {
		gate      *guestReferenceGate
		sessionID string
	}{gate: s.guestReference, sessionID: r.PathValue("session_id")}))
	next(w, request)
}
