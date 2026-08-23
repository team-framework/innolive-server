package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"inno-live-server/internal/session"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

func (s *Server) handleSignaling(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{CheckOrigin: func(request *http.Request) bool {
		return s.origins.Allows(request.Header.Get("Origin"))
	}}
	connection, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer connection.Close()

	for {
		_, data, err := connection.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				s.logger.Info("WebRTC signaling websocket disconnected", "error", err)
			}
			return
		}
		response, apiErr := s.handleSignalingMessage(data)
		if apiErr != nil {
			if err := connection.WriteJSON(signalingErrorResponse(*apiErr)); err != nil {
				return
			}
			continue
		}
		if err := connection.WriteJSON(response); err != nil {
			return
		}
	}
}

func (s *Server) handleSignalingMessage(data []byte) (any, *apiError) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil || payload == nil {
		result := badRequest("Signaling messages must be valid JSON objects.", nil)
		return nil, &result
	}
	var messageType string
	if raw, ok := payload["type"]; !ok || json.Unmarshal(raw, &messageType) != nil || messageType == "" {
		result := badRequest("Unsupported signaling message type.", map[string]any{"supported_types": []string{"offer", "ice_candidate"}})
		return nil, &result
	}
	switch messageType {
	case "offer":
		return s.handleOffer(payload)
	case "ice_candidate":
		return s.handleICECandidate(payload)
	default:
		result := badRequest("Unsupported signaling message type.", map[string]any{"type": messageType, "supported_types": []string{"offer", "ice_candidate"}})
		return nil, &result
	}
}

func (s *Server) handleOffer(payload map[string]json.RawMessage) (any, *apiError) {
	var request struct {
		SessionID     string `json:"session_id"`
		OwnerToken    string `json:"owner_token"`
		AccessToken   string `json:"access_token"`
		SDP           string `json:"sdp"`
		NegotiationID string `json:"negotiation_id"`
		ICERestart    bool   `json:"ice_restart"`
	}
	data, _ := json.Marshal(payload)
	if err := json.Unmarshal(data, &request); err != nil {
		result := badRequest("Invalid signaling message.", nil)
		return nil, &result
	}
	request.NegotiationID = strings.TrimSpace(request.NegotiationID)
	if strings.TrimSpace(request.SessionID) == "" || !validSDP(request.SDP) || (request.NegotiationID != "" && !validNegotiationID(request.NegotiationID)) || (request.ICERestart && request.NegotiationID == "") {
		result := badRequest("Invalid signaling message.", nil)
		return nil, &result
	}
	if apiErr := s.verifySignalingSession(context.Background(), strings.TrimSpace(request.SessionID), request.OwnerToken, request.AccessToken); apiErr != nil {
		return nil, apiErr
	}
	answer, err := s.sessions.CreateAnswerWithOptions(strings.TrimSpace(request.SessionID), request.OwnerToken, request.SDP, session.NegotiationOptions{
		NegotiationID: request.NegotiationID,
		ICERestart:    request.ICERestart,
	})
	if errors.Is(err, session.ErrNotFound) {
		result := apiError{Status: http.StatusNotFound, Code: "not_found", Message: "Session not found.", Details: map[string]any{"session_id": request.SessionID}}
		return nil, &result
	}
	if errors.Is(err, session.ErrUnauthorized) {
		result := apiError{Status: http.StatusForbidden, Code: "forbidden", Message: "Session owner token is invalid.", Details: map[string]any{"session_id": request.SessionID}}
		return nil, &result
	}
	if errors.Is(err, session.ErrRecoveryAttemptsExhausted) {
		result := apiError{Status: http.StatusConflict, Code: "peer_recovery_attempts_exhausted", Message: "The WebRTC recovery attempt limit has been reached.", Details: map[string]any{"session_id": request.SessionID}}
		return nil, &result
	}
	if err != nil {
		s.logger.Info("WebRTC offer rejected", "session_id", request.SessionID, "error", err)
		result := badRequest("Invalid WebRTC offer.", map[string]any{"session_id": request.SessionID})
		return nil, &result
	}
	return answer, nil
}

func (s *Server) handleICECandidate(payload map[string]json.RawMessage) (any, *apiError) {
	rawCandidate, hasCandidate := payload["candidate"]
	if !hasCandidate {
		result := badRequest("Invalid signaling message.", nil)
		return nil, &result
	}
	var request struct {
		SessionID     string  `json:"session_id"`
		OwnerToken    string  `json:"owner_token"`
		AccessToken   string  `json:"access_token"`
		Candidate     *string `json:"candidate"`
		SDPMid        *string `json:"sdpMid"`
		SDPLineIndex  *uint16 `json:"sdpMLineIndex"`
		NegotiationID string  `json:"negotiation_id"`
	}
	data, _ := json.Marshal(payload)
	if err := json.Unmarshal(data, &request); err != nil {
		result := badRequest("Invalid signaling message.", nil)
		return nil, &result
	}
	request.NegotiationID = strings.TrimSpace(request.NegotiationID)
	if strings.TrimSpace(request.SessionID) == "" || (request.NegotiationID != "" && !validNegotiationID(request.NegotiationID)) {
		result := badRequest("Invalid signaling message.", nil)
		return nil, &result
	}
	candidateValue := ""
	if string(rawCandidate) != "null" {
		if request.Candidate == nil {
			result := badRequest("Invalid ICE candidate.", nil)
			return nil, &result
		}
		candidateValue = strings.TrimSpace(*request.Candidate)
	}
	if candidateValue != "" && request.SDPMid == nil && request.SDPLineIndex == nil {
		result := badRequest("Invalid ICE candidate.", nil)
		return nil, &result
	}
	if apiErr := s.verifySignalingSession(context.Background(), strings.TrimSpace(request.SessionID), request.OwnerToken, request.AccessToken); apiErr != nil {
		return nil, apiErr
	}
	result, err := s.sessions.AddICECandidateWithNegotiation(strings.TrimSpace(request.SessionID), request.OwnerToken, request.NegotiationID, webrtc.ICECandidateInit{
		Candidate:     candidateValue,
		SDPMid:        request.SDPMid,
		SDPMLineIndex: request.SDPLineIndex,
	})
	if errors.Is(err, session.ErrNotFound) {
		apiErr := apiError{Status: http.StatusNotFound, Code: "not_found", Message: "Session not found.", Details: map[string]any{"session_id": request.SessionID}}
		return nil, &apiErr
	}
	if errors.Is(err, session.ErrUnauthorized) {
		apiErr := apiError{Status: http.StatusForbidden, Code: "forbidden", Message: "Session owner token is invalid.", Details: map[string]any{"session_id": request.SessionID}}
		return nil, &apiErr
	}
	if errors.Is(err, session.ErrStaleNegotiation) {
		apiErr := apiError{Status: http.StatusConflict, Code: "stale_negotiation", Message: "ICE candidate belongs to a stale negotiation.", Details: map[string]any{"session_id": request.SessionID}}
		return nil, &apiErr
	}
	if err != nil {
		s.logger.Info("ICE candidate rejected", "session_id", request.SessionID, "error", err)
		apiErr := badRequest("Invalid ICE candidate.", map[string]any{"session_id": request.SessionID})
		return nil, &apiErr
	}
	return result, nil
}

// verifySignalingSession binds each WebSocket action to the same active user
// that created the session, in addition to the session's one-time owner token.
// access_token is part of the encrypted WebSocket message because browsers
// cannot attach an Authorization header to WebSocket upgrade requests.
func (s *Server) verifySignalingSession(ctx context.Context, sessionID, ownerToken, accessToken string) *apiError {
	liveSession, err := s.sessions.VerifyOwner(sessionID, ownerToken)
	if errors.Is(err, session.ErrNotFound) {
		result := apiError{Status: http.StatusNotFound, Code: "not_found", Message: "Session not found.", Details: map[string]any{"session_id": sessionID}}
		return &result
	}
	if errors.Is(err, session.ErrUnauthorized) {
		result := apiError{Status: http.StatusForbidden, Code: "forbidden", Message: "Session owner token is invalid.", Details: map[string]any{"session_id": sessionID}}
		return &result
	}
	if err != nil {
		result := internalError()
		return &result
	}
	if s.authenticateUser == nil {
		return nil
	}
	userID, err := s.authenticateUser(ctx, strings.TrimSpace(accessToken))
	if err != nil {
		result := apiError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "Authentication is required."}
		return &result
	}
	if !sameSessionUser(liveSession.UserID, userID) {
		result := apiError{Status: http.StatusForbidden, Code: "forbidden", Message: "Session does not belong to the authenticated user.", Details: map[string]any{"session_id": sessionID}}
		return &result
	}
	return nil
}

func sameSessionUser(owner, caller uuid.UUID) bool {
	return owner != uuid.Nil && owner == caller
}

func signalingErrorResponse(err apiError) map[string]any {
	payload := map[string]any{"code": err.Code, "message": err.Message}
	if len(err.Details) > 0 {
		payload["details"] = err.Details
	}
	return map[string]any{"type": "error", "error": payload}
}

func validSDP(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "v=0") && (strings.Contains(value, "\nm=") || strings.Contains(value, "\r\nm="))
}

func validNegotiationID(value string) bool {
	_, err := uuid.Parse(strings.TrimSpace(value))
	return err == nil
}
