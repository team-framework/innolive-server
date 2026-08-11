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

// TestAnswerRetainsOfferedRTCPFeedback guards the RTCP feedback the client
// offers. Rebuilding the codec from the offer SDP drops any RTCPFeedback that
// prepareOutputForOffer does not copy over, and SetCodecPreferences then
// replaces the transceiver's full codec capability — so an empty list silently
// strips every a=rtcp-fb line from the answer. Losing "nack" removes the only
// packet-recovery path and losing "nack pli" removes the client's only way to
// ask for a keyframe, which corrupts the decoder's static regions until the
// encoder's next natural IDR.
func TestAnswerRetainsOfferedRTCPFeedback(t *testing.T) {
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

	offered := videoFeedbackLines(offer.SDP)
	answered := videoFeedbackLines(answer.SDP)
	for _, feedback := range []string{"nack", "nack pli", "ccm fir", "goog-remb", "transport-cc"} {
		if offered[feedback] == 0 {
			t.Fatalf("offer is missing %q feedback, test setup is wrong:\n%s", feedback, offer.SDP)
		}
		if answered[feedback] == 0 {
			t.Errorf("answer dropped %q feedback offered by the client", feedback)
		}
	}
	if t.Failed() {
		t.Logf("answer m=video section:\n%s", strings.Join(videoSectionLines(answer.SDP), "\n"))
	}
}

// videoSectionLines returns the lines of the first m=video section of an SDP.
func videoSectionLines(sdp string) []string {
	var lines []string
	inVideo := false
	for _, line := range strings.Split(sdp, "\r\n") {
		if strings.HasPrefix(line, "m=") {
			if inVideo {
				break
			}
			inVideo = strings.HasPrefix(line, "m=video")
		}
		if inVideo {
			lines = append(lines, line)
		}
	}
	return lines
}

// videoFeedbackLines counts a=rtcp-fb lines in the video section by feedback
// type, keyed the way they appear in SDP ("nack", "nack pli", "ccm fir", ...).
func videoFeedbackLines(sdp string) map[string]int {
	counts := make(map[string]int)
	for _, line := range videoSectionLines(sdp) {
		_, attribute, found := strings.Cut(line, "a=rtcp-fb:")
		if !found {
			continue
		}
		_, feedback, found := strings.Cut(attribute, " ")
		if !found {
			continue
		}
		counts[strings.TrimSpace(feedback)]++
	}
	return counts
}
