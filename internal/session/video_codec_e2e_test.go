package session

import (
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
)

// TestConnectedPeerNegotiatesRTCPFeedback은 TestAnswerRetainsOfferedRTCPFeedback의
// 런타임 짝이다. 매니저와 실제 ICE + DTLS 핸드셰이크를 끝내고 클라이언트가 최종
// 적용한 코덱 파라미터를 단언한다. Pion은 SDP 텍스트가 아니라 이 협상된
// 파라미터로 NACK·PLI·transport-cc 인터셉터를 돌릴지 정하므로, 라이브 세션에서
// 피드백이 실제로 동작하는지를 정하는 것이 이것이다.
func TestConnectedPeerNegotiatesRTCPFeedback(t *testing.T) {
	manager := newTestManager(t, 0)
	liveSession, ownerToken, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	capability := webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		RTCPFeedback: []webrtc.RTCPFeedback{
			{Type: "goog-remb"},
			{Type: "ccm", Parameter: "fir"},
			{Type: "nack"},
			{Type: "nack", Parameter: "pli"},
			{Type: "transport-cc"},
		},
	}
	track, err := webrtc.NewTrackLocalStaticSample(capability, "camera", "client")
	if err != nil {
		t.Fatal(err)
	}
	transceiver, err := client.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	if err != nil {
		t.Fatal(err)
	}
	if err = transceiver.SetCodecPreferences([]webrtc.RTPCodecParameters{{RTPCodecCapability: capability}}); err != nil {
		t.Fatal(err)
	}

	connected := make(chan struct{})
	var once sync.Once
	client.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	})

	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(client)
	if err = client.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered

	answer, err := manager.CreateAnswer(liveSession.ID, ownerToken, client.LocalDescription().SDP)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		t.Fatalf("peer connection never connected (ice=%s, state=%s)", client.ICEConnectionState(), client.ConnectionState())
	}

	// 실제 연결 뒤에도 같은 PeerConnection에 ICE restart offer를 적용할 수
	// 있어야 네트워크 전환 시 세션·송출 egress를 새로 만들지 않는다.
	restartOffer, err := client.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		t.Fatalf("create ICE restart offer: %v", err)
	}
	restartGathered := webrtc.GatheringCompletePromise(client)
	if err = client.SetLocalDescription(restartOffer); err != nil {
		t.Fatalf("set local ICE restart offer: %v", err)
	}
	<-restartGathered
	negotiationID := uuid.NewString()
	restartAnswer, err := manager.CreateAnswerWithOptions(liveSession.ID, ownerToken, client.LocalDescription().SDP, NegotiationOptions{
		NegotiationID: negotiationID,
		ICERestart:    true,
	})
	if err != nil {
		t.Fatalf("create ICE restart answer: %v", err)
	}
	if restartAnswer.NegotiationID != negotiationID {
		t.Fatalf("restart answer negotiation ID = %q, want %q", restartAnswer.NegotiationID, negotiationID)
	}
	if err = client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: restartAnswer.SDP}); err != nil {
		t.Fatalf("set remote ICE restart answer: %v", err)
	}

	want := []string{"ccm fir", "goog-remb", "nack", "nack pli", "transport-cc"}
	for _, direction := range []struct {
		name   string
		codecs []webrtc.RTPCodecParameters
	}{
		{"receiver", transceiver.Receiver().GetParameters().Codecs},
		{"sender", transceiver.Sender().GetParameters().Codecs},
	} {
		if len(direction.codecs) == 0 {
			t.Errorf("%s negotiated no codec at all", direction.name)
			continue
		}
		got := negotiatedFeedback(direction.codecs[0].RTCPFeedback)
		for _, feedback := range want {
			if !got[feedback] {
				t.Errorf("%s codec %s did not negotiate %q feedback (got %v)",
					direction.name, direction.codecs[0].MimeType, feedback, sortedKeys(got))
			}
		}
	}
}

// negotiatedFeedback은 RTCPFeedback을 SDP에 나오는 모양("nack pli")으로 키를 잡는다.
func negotiatedFeedback(feedback []webrtc.RTCPFeedback) map[string]bool {
	present := make(map[string]bool, len(feedback))
	for _, item := range feedback {
		if item.Parameter == "" {
			present[item.Type] = true
			continue
		}
		present[item.Type+" "+item.Parameter] = true
	}
	return present
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
