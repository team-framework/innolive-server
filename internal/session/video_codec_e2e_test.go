package session

import (
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// TestConnectedPeerNegotiatesRTCPFeedback is the runtime counterpart to
// TestAnswerRetainsOfferedRTCPFeedback: it completes a real ICE + DTLS
// handshake against the manager and asserts on the codec parameters the client
// ends up applying. Pion decides whether to run its NACK, PLI and transport-cc
// interceptors from these negotiated parameters, not from the SDP text, so this
// is what actually determines whether feedback works on a live session.
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

// negotiatedFeedback keys RTCPFeedback the way it appears in SDP ("nack pli").
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
