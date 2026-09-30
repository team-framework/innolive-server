package session

import (
	"fmt"
	"strconv"
	"strings"

	"inno-live-server/internal/media"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// prepareOutputForOffer는 aiortc의 answer 쪽 동작을 따른다. offer의 영상 payload
// type을 원래 순서대로 훑어 클라이언트와 이 서버가 모두 쓸 수 있는 첫 코덱을 묶는다.
// 서버 쪽 H.264 선호를 일부러 강요하지 않는다.
func (s *Session) prepareOutputForOffer(offerSDP string) error {
	if s.Output != nil || s.Sender != nil {
		return nil
	}
	transceiver := firstVideoTransceiver(s.PC)
	if transceiver == nil {
		return fmt.Errorf("remote offer has no video transceiver")
	}
	candidates, err := offeredVideoCodecs(offerSDP)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if err := transceiver.SetCodecPreferences([]webrtc.RTPCodecParameters{candidate.parameters}); err != nil {
			continue
		}
		output, err := webrtc.NewTrackLocalStaticSample(candidate.parameters.RTPCodecCapability, "processed-video", s.ID)
		if err != nil {
			return fmt.Errorf("create %s processed video track: %w", candidate.codec, err)
		}
		sender, err := s.PC.AddTrack(output)
		if err != nil {
			return fmt.Errorf("add %s processed video track: %w", candidate.codec, err)
		}
		s.mu.Lock()
		s.Output = output
		s.Sender = sender
		s.VideoCodec = candidate.codec
		s.mu.Unlock()
		go drainRTCP(sender)
		return nil
	}
	return fmt.Errorf("remote offer has no supported H.264 or VP8 video codec")
}

type offeredVideoCodec struct {
	codec      media.VideoCodec
	parameters webrtc.RTPCodecParameters
}

func offeredVideoCodecs(raw string) ([]offeredVideoCodec, error) {
	var description sdp.SessionDescription
	if err := description.UnmarshalString(raw); err != nil {
		return nil, fmt.Errorf("parse remote offer SDP: %w", err)
	}
	var result []offeredVideoCodec
	for _, mediaDescription := range description.MediaDescriptions {
		if !strings.EqualFold(mediaDescription.MediaName.Media, "video") {
			continue
		}
		feedback := offeredRTCPFeedback(mediaDescription)
		for _, format := range mediaDescription.MediaName.Formats {
			payloadType, err := strconv.ParseUint(format, 10, 8)
			if err != nil {
				continue
			}
			codec, err := description.GetCodecForPayloadType(uint8(payloadType))
			if err != nil || codec.ClockRate != 90000 {
				continue
			}
			videoCodec := media.VideoCodec("video/" + strings.ToUpper(codec.Name))
			if !videoCodec.Valid() {
				continue
			}
			result = append(result, offeredVideoCodec{
				codec: videoCodec,
				parameters: webrtc.RTPCodecParameters{
					RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: string(videoCodec), ClockRate: codec.ClockRate, SDPFmtpLine: codec.Fmtp, RTCPFeedback: feedback[uint8(payloadType)]},
					PayloadType:        webrtc.PayloadType(payloadType),
				},
			})
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("remote offer has no H.264 or VP8 video codec")
	}
	return result, nil
}

// offeredRTCPFeedback은 미디어 섹션의 a=rtcp-fb 줄을 payload type별로 모은다.
// SetCodecPreferences는 트랜시버의 코덱 능력을 통째로 덮어쓰므로, offer로 다시 만든
// 코덱은 클라이언트가 요청한 피드백을 실어야 한다. 아니면 answer가 피드백을 하나도
// 알리지 않는다.
func offeredRTCPFeedback(mediaDescription *sdp.MediaDescription) map[uint8][]webrtc.RTCPFeedback {
	feedback := make(map[uint8][]webrtc.RTCPFeedback)
	for _, attribute := range mediaDescription.Attributes {
		if attribute.Key != "rtcp-fb" {
			continue
		}
		format, value, found := strings.Cut(attribute.Value, " ")
		if !found {
			continue
		}
		payloadType, err := strconv.ParseUint(format, 10, 8)
		if err != nil {
			continue
		}
		feedbackType, parameter, _ := strings.Cut(strings.TrimSpace(value), " ")
		feedback[uint8(payloadType)] = append(feedback[uint8(payloadType)], webrtc.RTCPFeedback{Type: feedbackType, Parameter: parameter})
	}
	return feedback
}

func firstVideoTransceiver(pc *webrtc.PeerConnection) *webrtc.RTPTransceiver {
	for _, transceiver := range pc.GetTransceivers() {
		if transceiver.Kind() == webrtc.RTPCodecTypeVideo {
			return transceiver
		}
	}
	return nil
}
