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

// LocalICECandidate는 서버가 trickle ICE로 클라이언트에 전달하는 후보 하나다.
// Candidate가 nil이면 해당 negotiation 세대의 후보 수집이 완료됐다는 뜻이다.
type LocalICECandidate struct {
	Type          string  `json:"type"`
	SessionID     string  `json:"session_id"`
	NegotiationID string  `json:"negotiation_id,omitempty"`
	Candidate     *string `json:"candidate"`
	SDPMid        *string `json:"sdpMid,omitempty"`
	SDPLineIndex  *uint16 `json:"sdpMLineIndex,omitempty"`
}

// LocalCandidateHandler는 세션 계층이 signaling 전송 기술을 모르도록 유지한
// 서버 후보 전달 계약이다. callback은 Session.mu 밖에서 호출돼 WebSocket
// backpressure가 세션 상태 변경을 막지 않는다.
type LocalCandidateHandler func(LocalICECandidate)

// NegotiationOptions는 하나의 offer·candidate 세대를 식별한다. ICE restart는
// 기존 PeerConnection을 유지한 채 후보를 새로 수집하므로, 이전 세대 후보가 늦게
// 도착해 새 협상을 오염시키지 않도록 negotiation ID를 함께 보낸다.
type NegotiationOptions struct {
	NegotiationID       string
	ICERestart          bool
	OnLocalICECandidate LocalCandidateHandler
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
	trickle := options.NegotiationID != "" && options.OnLocalICECandidate != nil
	if options.NegotiationID != "" {
		s.mu.Lock()
		s.activeNegotiationID = options.NegotiationID
		s.UpdatedAt = time.Now().UTC()
		s.mu.Unlock()
		if trickle {
			// Pion은 후보를 발견한 시점의 OnICECandidate callback을 호출한다.
			// callback 안에서 Session의 가변 current generation을 다시 읽으면,
			// 이전 gathering의 지연 callback이 새 ICE restart ID로 재표시될 수 있다.
			// 따라서 offer마다 generation·handler를 closure로 캡처한다.
			s.bindLocalICECandidateHandler(options.NegotiationID, options.OnLocalICECandidate)
		}
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
	var gatheringComplete <-chan struct{}
	if !trickle {
		// 직접 Manager API를 쓰는 기존 호출자는 후보를 별도 전달받을 방법이
		// 없으므로, 이전처럼 완성된 SDP answer를 유지한다. WebSocket signaling
		// 경로만 OnLocalICECandidate를 주입해 trickle ICE로 동작한다.
		gatheringComplete = webrtc.GatheringCompletePromise(s.PC)
	}
	if err := s.PC.SetLocalDescription(answer); err != nil {
		return Answer{}, fmt.Errorf("set local answer: %w", err)
	}
	if !trickle {
		<-gatheringComplete
	}
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

// bindLocalICECandidateHandler는 Pion callback에 해당 offer의 generation을
// closure로 고정한다. 이전 gathering에서 이미 읽힌 callback이 뒤늦게 실행돼도
// 새 ICE restart generation으로 재표시되지 않는다.
func (s *Session) bindLocalICECandidateHandler(negotiationID string, handler LocalCandidateHandler) {
	s.PC.OnICECandidate(s.localICECandidateDispatcher(negotiationID, handler))
}

// localICECandidateDispatcher는 테스트 가능한 generation 고정 callback을 만든다.
// Pion은 후보 수집 완료를 nil로 전달하며, 이 경우에도 callback을 만든 순간의
// negotiation ID가 유지돼야 한다.
func (s *Session) localICECandidateDispatcher(negotiationID string, handler LocalCandidateHandler) func(*webrtc.ICECandidate) {
	return func(candidate *webrtc.ICECandidate) {
		s.dispatchLocalICECandidate(negotiationID, handler, candidate)
	}
}

// dispatchLocalICECandidate는 generation이 고정된 서버 후보를 signaling 계층으로
// 전달한다. handler 호출은 Session.mu 밖에서 수행해 WebSocket backpressure가
// 세션 상태 변경을 막지 않게 한다.
func (s *Session) dispatchLocalICECandidate(negotiationID string, handler LocalCandidateHandler, candidate *webrtc.ICECandidate) {
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if handler == nil || negotiationID == "" || closed {
		return
	}

	message := LocalICECandidate{
		Type:          "ice_candidate",
		SessionID:     s.ID,
		NegotiationID: negotiationID,
	}
	if candidate != nil {
		encoded := candidate.ToJSON()
		candidateValue := encoded.Candidate
		message.Candidate = &candidateValue
		message.SDPMid = encoded.SDPMid
		message.SDPLineIndex = encoded.SDPMLineIndex
	}
	handler(message)
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
