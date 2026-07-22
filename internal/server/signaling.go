package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"inno-live-server/internal/session"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

var signalingUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func (s *Server) handleSignaling(w http.ResponseWriter, r *http.Request) {
	connection, err := signalingUpgrader.Upgrade(w, r, nil)
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
		SessionID string `json:"session_id"`
		SDP       string `json:"sdp"`
	}
	data, _ := json.Marshal(payload)
	if err := json.Unmarshal(data, &request); err != nil || strings.TrimSpace(request.SessionID) == "" || !validSDP(request.SDP) {
		result := badRequest("Invalid signaling message.", nil)
		return nil, &result
	}
	answer, err := s.sessions.CreateAnswer(strings.TrimSpace(request.SessionID), request.SDP)
	if errors.Is(err, session.ErrNotFound) {
		result := apiError{Status: http.StatusNotFound, Code: "not_found", Message: "Session not found.", Details: map[string]any{"session_id": request.SessionID}}
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
		SessionID    string  `json:"session_id"`
		Candidate    *string `json:"candidate"`
		SDPMid       *string `json:"sdpMid"`
		SDPLineIndex *uint16 `json:"sdpMLineIndex"`
	}
	data, _ := json.Marshal(payload)
	if err := json.Unmarshal(data, &request); err != nil || strings.TrimSpace(request.SessionID) == "" {
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
	result, err := s.sessions.AddICECandidate(strings.TrimSpace(request.SessionID), webrtc.ICECandidateInit{
		Candidate:     candidateValue,
		SDPMid:        request.SDPMid,
		SDPMLineIndex: request.SDPLineIndex,
	})
	if errors.Is(err, session.ErrNotFound) {
		apiErr := apiError{Status: http.StatusNotFound, Code: "not_found", Message: "Session not found.", Details: map[string]any{"session_id": request.SessionID}}
		return nil, &apiErr
	}
	if err != nil {
		s.logger.Info("ICE candidate rejected", "session_id", request.SessionID, "error", err)
		apiErr := badRequest("Invalid ICE candidate.", map[string]any{"session_id": request.SessionID})
		return nil, &apiErr
	}
	return result, nil
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
