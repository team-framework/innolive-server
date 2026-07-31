package session

import (
	"fmt"
	"strconv"
	"strings"

	"inno-live-server/internal/media"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// prepareOutputForOffer mirrors aiortc's answer-side behaviour: walk the
// offered video payload types in their original order and bind the first codec
// that both the client and this server can use. It deliberately does not impose
// a server H.264 preference.
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
					RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: string(videoCodec), ClockRate: codec.ClockRate, SDPFmtpLine: codec.Fmtp},
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

func firstVideoTransceiver(pc *webrtc.PeerConnection) *webrtc.RTPTransceiver {
	for _, transceiver := range pc.GetTransceivers() {
		if transceiver.Kind() == webrtc.RTPCodecTypeVideo {
			return transceiver
		}
	}
	return nil
}
