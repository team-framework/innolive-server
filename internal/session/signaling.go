package session

import (
	"fmt"
	"strings"
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

// LocalICECandidate는 서버가 trickle ICE로 클라이언트에 전달하는 실제 후보 하나다.
// Pion의 후보 수집 완료 nil 신호는 generation을 안전하게 판별할 수 없으므로 원격에
// 전달하지 않는다.
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

type localCandidateRoute struct {
	negotiationID string
	handler       LocalCandidateHandler
}

const maxLocalCandidateRoutes = 2

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
	if trickle {
		// Pion은 OnICECandidate callback을 바꾸는 시점이 아니라 내부 queue에서
		// notification을 꺼내는 시점에 handler를 읽는다. 따라서 generation별
		// handler 교체는 이전 candidate를 새 generation으로 재표시할 수 있다.
		// 답변 SDP의 local ufrag와 handler를 먼저 등록하고, 고정 callback이 각
		// candidate 안의 ufrag로 올바른 route를 찾게 한다.
		if err := s.registerLocalCandidateRoute(answer.SDP, options.NegotiationID, options.OnLocalICECandidate); err != nil {
			return Answer{}, err
		}
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

// registerLocalCandidateRoute는 answer SDP에 있는 local ICE ufrag를 해당 offer의
// signaling route에 연결한다. 가장 최근 두 generation만 보관하고, 더 오래된
// queue 후보는 전송하지 않아도 새 generation으로 잘못 표기하지 않는다.
func (s *Session) registerLocalCandidateRoute(answerSDP, negotiationID string, handler LocalCandidateHandler) error {
	ufrags := localICEUsernameFragments(answerSDP)
	if len(ufrags) == 0 {
		return fmt.Errorf("answer SDP is missing local ICE username fragment")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.localCandidateRoutes == nil {
		s.localCandidateRoutes = make(map[string]localCandidateRoute)
	}
	for _, ufrag := range ufrags {
		for index, existing := range s.localCandidateRouteOrder {
			if existing == ufrag {
				s.localCandidateRouteOrder = append(s.localCandidateRouteOrder[:index], s.localCandidateRouteOrder[index+1:]...)
				break
			}
		}
		s.localCandidateRoutes[ufrag] = localCandidateRoute{negotiationID: negotiationID, handler: handler}
		s.localCandidateRouteOrder = append(s.localCandidateRouteOrder, ufrag)
	}
	for len(s.localCandidateRouteOrder) > maxLocalCandidateRoutes {
		oldest := s.localCandidateRouteOrder[0]
		s.localCandidateRouteOrder = s.localCandidateRouteOrder[1:]
		delete(s.localCandidateRoutes, oldest)
	}
	return nil
}

// dispatchLocalICECandidate는 Pion이 실제로 생성한 candidate의 ufrag로 route를
// 찾는다. Pion queue가 이전 generation candidate를 새 offer 뒤에 callback으로
// 넘겨도, candidate 자체의 ufrag가 이전 route를 선택한다. nil 완료 신호는 ufrag가
// 없어서 세대를 판별할 수 없으므로 원격에 전달하지 않는다.
func (s *Session) dispatchLocalICECandidate(candidate *webrtc.ICECandidate) {
	if candidate == nil {
		return
	}
	encoded := candidate.ToJSON()
	ufrag := ""
	if encoded.UsernameFragment != nil {
		ufrag = *encoded.UsernameFragment
	}
	if ufrag == "" {
		ufrag = localCandidateUsernameFragment(encoded.Candidate)
	}
	s.mu.RLock()
	route, ok := s.localCandidateRoutes[ufrag]
	closed := s.closed
	s.mu.RUnlock()
	if !ok || route.handler == nil || route.negotiationID == "" || closed {
		return
	}

	message := LocalICECandidate{
		Type:          "ice_candidate",
		SessionID:     s.ID,
		NegotiationID: route.negotiationID,
	}
	candidateValue := encoded.Candidate
	message.Candidate = &candidateValue
	message.SDPMid = encoded.SDPMid
	message.SDPLineIndex = encoded.SDPMLineIndex
	route.handler(message)
}

func localICEUsernameFragments(sessionDescription string) []string {
	seen := make(map[string]struct{})
	var fragments []string
	for _, line := range strings.Split(sessionDescription, "\n") {
		const prefix = "a=ice-ufrag:"
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		fragment := strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
		if fragment == "" {
			continue
		}
		if _, exists := seen[fragment]; exists {
			continue
		}
		seen[fragment] = struct{}{}
		fragments = append(fragments, fragment)
	}
	return fragments
}

func localCandidateUsernameFragment(candidate string) string {
	fields := strings.Fields(strings.TrimPrefix(candidate, "candidate:"))
	for index := 0; index+1 < len(fields); index++ {
		if fields[index] == "ufrag" {
			return fields[index+1]
		}
	}
	return ""
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
