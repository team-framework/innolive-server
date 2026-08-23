package session

import (
	"fmt"
	"time"

	"github.com/pion/webrtc/v4"
)

type Answer struct {
	SessionID     string `json:"session_id"`
	Type          string `json:"type"`
	SDP           string `json:"sdp"`
	NegotiationID string `json:"negotiation_id,omitempty"`
}

type CandidateResult struct {
	Type               string `json:"type"`
	SessionID          string `json:"session_id"`
	NegotiationID      string `json:"negotiation_id,omitempty"`
	EndOfCandidates    bool   `json:"end_of_candidates"`
	Queued             bool   `json:"queued"`
	ICEConnectionState string `json:"ice_connection_state"`
	ConnectionState    string `json:"connection_state"`
}

// NegotiationOptions는 하나의 offer·candidate 세대를 식별한다. ICE restart는
// 기존 PeerConnection을 유지한 채 후보를 새로 수집하므로, 이전 세대 후보가 늦게
// 도착해 새 협상을 오염시키지 않도록 negotiation ID를 함께 보낸다.
type NegotiationOptions struct {
	NegotiationID string
	ICERestart    bool
}

func (m *Manager) CreateAnswer(sessionID, ownerToken, offerSDP string) (Answer, error) {
	return m.CreateAnswerWithOptions(sessionID, ownerToken, offerSDP, NegotiationOptions{})
}

func (m *Manager) CreateAnswerWithOptions(sessionID, ownerToken, offerSDP string, options NegotiationOptions) (Answer, error) {
	s, err := m.VerifyOwner(sessionID, ownerToken)
	if err != nil {
		return Answer{}, err
	}
	s.negotiationMu.Lock()
	defer s.negotiationMu.Unlock()

	s.mu.Lock()
	if s.closed || s.Status != "active" {
		s.mu.Unlock()
		return Answer{}, fmt.Errorf("session is not active")
	}
	if s.PC.SignalingState() != webrtc.SignalingStateStable {
		state := s.PC.SignalingState().String()
		s.mu.Unlock()
		return Answer{}, fmt.Errorf("PeerConnection is not stable: %s", state)
	}
	if options.ICERestart {
		s.mu.Unlock()
		if err := m.registerRecoveryOffer(s); err != nil {
			return Answer{}, err
		}
		s.mu.Lock()
		if s.closed || s.Status != "active" || s.PC.SignalingState() != webrtc.SignalingStateStable {
			s.mu.Unlock()
			return Answer{}, fmt.Errorf("session is no longer ready for ICE restart")
		}
	}
	now := time.Now().UTC()
	s.offerReceivedAt = now
	s.Timing.SessionToOfferMS = millisecondsPtr(now.Sub(s.CreatedAt))
	s.UpdatedAt = now
	s.mu.Unlock()

	if err := s.PC.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		return Answer{}, fmt.Errorf("set remote offer: %w", err)
	}
	if options.NegotiationID != "" {
		s.mu.Lock()
		s.activeNegotiationID = options.NegotiationID
		s.UpdatedAt = time.Now().UTC()
		s.mu.Unlock()
	}
	if err := s.prepareOutputForOffer(offerSDP); err != nil {
		return Answer{}, err
	}
	if err := s.applyPendingICE(); err != nil {
		return Answer{}, err
	}
	answer, err := s.PC.CreateAnswer(nil)
	if err != nil {
		return Answer{}, fmt.Errorf("create WebRTC answer: %w", err)
	}
	gatheringComplete := webrtc.GatheringCompletePromise(s.PC)
	if err := s.PC.SetLocalDescription(answer); err != nil {
		return Answer{}, fmt.Errorf("set local answer: %w", err)
	}
	<-gatheringComplete
	local := s.PC.LocalDescription()
	if local == nil {
		return Answer{}, fmt.Errorf("local WebRTC answer is missing")
	}

	s.mu.Lock()
	now = time.Now().UTC()
	s.answerCreatedAt = now
	s.Timing.OfferToAnswerMS = millisecondsPtr(now.Sub(s.offerReceivedAt))
	s.UpdatedAt = now
	s.mu.Unlock()
	m.logger.Info("created WebRTC answer", "session_id", sessionID, "offer_to_answer_ms", now.Sub(s.offerReceivedAt).Milliseconds())
	return Answer{SessionID: sessionID, Type: "answer", SDP: local.SDP, NegotiationID: options.NegotiationID}, nil
}

func (m *Manager) AddICECandidate(sessionID, ownerToken string, candidate webrtc.ICECandidateInit) (CandidateResult, error) {
	return m.AddICECandidateWithNegotiation(sessionID, ownerToken, "", candidate)
}

func (m *Manager) AddICECandidateWithNegotiation(sessionID, ownerToken, negotiationID string, candidate webrtc.ICECandidateInit) (CandidateResult, error) {
	s, err := m.VerifyOwner(sessionID, ownerToken)
	if err != nil {
		return CandidateResult{}, err
	}
	s.negotiationMu.Lock()
	defer s.negotiationMu.Unlock()

	s.mu.Lock()
	activeNegotiationID := s.activeNegotiationID
	if activeNegotiationID != "" && negotiationID != activeNegotiationID {
		s.mu.Unlock()
		return CandidateResult{}, ErrStaleNegotiation
	}
	s.mu.Unlock()

	queued := s.PC.RemoteDescription() == nil
	if queued {
		s.mu.Lock()
		s.pendingICE = append(s.pendingICE, candidate)
		s.UpdatedAt = time.Now().UTC()
		s.mu.Unlock()
	} else if err := s.PC.AddICECandidate(candidate); err != nil {
		return CandidateResult{}, fmt.Errorf("add ICE candidate: %w", err)
	}
	m.logger.Info("received remote ICE candidate", "session_id", sessionID, "queued", queued, "end_of_candidates", candidate.Candidate == "", "candidate", candidate.Candidate)
	return CandidateResult{
		Type:               "ice_candidate_added",
		SessionID:          sessionID,
		NegotiationID:      negotiationID,
		EndOfCandidates:    candidate.Candidate == "",
		Queued:             queued,
		ICEConnectionState: s.PC.ICEConnectionState().String(),
		ConnectionState:    s.PC.ConnectionState().String(),
	}, nil
}

func (s *Session) applyPendingICE() error {
	s.mu.Lock()
	pending := append([]webrtc.ICECandidateInit(nil), s.pendingICE...)
	s.pendingICE = nil
	s.mu.Unlock()
	for _, candidate := range pending {
		if err := s.PC.AddICECandidate(candidate); err != nil {
			return fmt.Errorf("add queued ICE candidate: %w", err)
		}
	}
	return nil
}

func drainRTCP(sender *webrtc.RTPSender) {
	for {
		if _, _, err := sender.ReadRTCP(); err != nil {
			return
		}
	}
}

func drainReceiverRTCP(receiver *webrtc.RTPReceiver) {
	for {
		if _, _, err := receiver.ReadRTCP(); err != nil {
			return
		}
	}
}
