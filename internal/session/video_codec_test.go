package session

import (
	"strings"
	"testing"

	"inno-live-server/internal/media"

	"github.com/pion/webrtc/v4"
)

func TestOfferedVideoCodecsPreservesClientOrder(t *testing.T) {
	const offer = "v=0\r\n" +
		"o=- 1 1 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 102 96\r\n" +
		"a=rtpmap:102 H264/90000\r\n" +
		"a=fmtp:102 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f\r\n" +
		"a=rtpmap:96 VP8/90000\r\n"
	codecs, err := offeredVideoCodecs(offer)
	if err != nil {
		t.Fatal(err)
	}
	if len(codecs) != 2 || codecs[0].codec != media.VideoCodecH264 || codecs[1].codec != media.VideoCodecVP8 {
		t.Fatalf("offered codecs = %#v", codecs)
	}
}

func TestCreateAnswerSelectsClientPreferredH264(t *testing.T) {
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
	transceiver, err := client.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
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
}
