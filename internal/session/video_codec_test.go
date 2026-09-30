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

// TestAnswerRetainsOfferedRTCPFeedback은 클라이언트가 offer한 RTCP 피드백을 지킨다.
// offer SDP로 코덱을 다시 만들면 prepareOutputForOffer가 옮기지 않은 RTCPFeedback이
// 빠지고, SetCodecPreferences가 트랜시버의 코덱 능력을 통째로 바꾸므로 빈 목록이면
// answer에서 a=rtcp-fb 줄이 조용히 모두 사라진다. "nack"를 잃으면 유일한 패킷 복구
// 경로가, "nack pli"를 잃으면 클라이언트가 키프레임을 요청할 유일한 방법이 사라져
// 인코더의 다음 자연 IDR까지 디코더의 정적 영역이 깨진다.
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

// videoSectionLines는 SDP의 첫 m=video 섹션 줄을 돌려준다.
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

// videoFeedbackLines는 영상 섹션의 a=rtcp-fb 줄을 피드백 종류별로 센다. 키는 SDP에
// 나오는 모양("nack", "nack pli", "ccm fir", ...)이다.
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
