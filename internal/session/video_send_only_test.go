package session

import (
	"github.com/pion/webrtc/v4"
	"inno-live-server/internal/media"
	"strings"
	"testing"
)

func TestSendOnlyPublisherKeepsH264AndDoesNotNegotiateReturn(t *testing.T) {
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
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
		"camera", "client",
	)
	if err != nil {
		t.Fatal(err)
	}
	transceiver, err := client.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		t.Fatal(err)
	}
	if err = transceiver.SetCodecPreferences([]webrtc.RTPCodecParameters{{RTPCodecCapability: track.Codec()}}); err != nil {
		t.Fatal(err)
	}
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	answer, err := manager.CreateAnswer(liveSession.ID, ownerToken, offer.SDP)
	if err != nil {
		t.Fatal(err)
	}
	if liveSession.VideoCodec != media.VideoCodecH264 {
		t.Fatalf("selected codec = %q, want H.264", liveSession.VideoCodec)
	}
	if !strings.Contains(answer.SDP, "H264/90000") || strings.Contains(answer.SDP, "VP8/90000") {
		t.Fatalf("answer did not retain the H.264-only offer:\n%s", answer.SDP)
	}
	if !strings.Contains(strings.Join(videoSectionLines(answer.SDP), "\n"), "a=recvonly") {
		t.Fatalf("answer direction lines: %v", videoSectionDirections(answer.SDP))
	}
	if err := client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatal(err)
	}
}

func videoSectionDirections(raw string) []string {
	var result []string
	for _, line := range videoSectionLines(raw) {
		if line == "a=recvonly" || line == "a=sendonly" || line == "a=inactive" || line == "a=sendrecv" {
			result = append(result, line)
		}
	}
	return result
}

func TestVideoOfferReturnDirection(t *testing.T) {
	for _, test := range []struct {
		name, session, video string
		receives             bool
	}{
		{"default", "", "", true}, {"sendrecv", "", "a=sendrecv\r\n", true},
		{"sendonly", "", "a=sendonly\r\n", false}, {"inactive", "", "a=inactive\r\n", false},
		{"recvonly", "", "a=recvonly\r\n", true}, {"inherit", "a=sendonly\r\n", "", false},
		{"override", "a=sendonly\r\n", "a=sendrecv\r\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n" + test.session + "m=video 9 UDP/TLS/RTP/SAVPF 96\r\na=rtpmap:96 VP8/90000\r\n" + test.video
			got, err := offeredVideoReceives(raw)
			if err != nil || got != test.receives {
				t.Fatalf("receives=%v err=%v, want %v", got, err, test.receives)
			}
		})
	}
}
