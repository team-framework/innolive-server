package media

import (
	"context"
	"strings"
	"testing"

	"inno-live-server/internal/config"
)

// TestDecoderOutputPinOff는 핀이 꺼져 있을 때 소스 치수를 그대로 쓰고 필터를
// 만들지 않는지 고정한다. 기본값이 OFF이므로 이 성질이 "머지해도 아무것도
// 바뀌지 않는다"는 보장 그 자체다(#122).
func TestDecoderOutputPinOff(t *testing.T) {
	transcoder := NewFFmpegTranscoder("ffmpeg", testLogger(), nil, TranscoderOptions{})
	for _, size := range [][2]uint16{{640, 360}, {270, 480}, {1920, 1080}} {
		width, height, filter, err := transcoder.decoderOutput(size[0], size[1])
		if err != nil {
			t.Fatalf("decoderOutput(%d, %d): %v", size[0], size[1], err)
		}
		if width != size[0] || height != size[1] {
			t.Errorf("decoderOutput(%d, %d) = %dx%d, want 소스 그대로", size[0], size[1], width, height)
		}
		if filter != "" {
			t.Errorf("핀이 꺼졌는데 필터가 생겼다: %q", filter)
		}
	}
}

// TestDecoderOutputPinOn은 핀이 켜졌을 때 방향에 따라 치수가 유도되는지 본다.
func TestDecoderOutputPinOn(t *testing.T) {
	transcoder := NewFFmpegTranscoder("ffmpeg", testLogger(), nil, TranscoderOptions{PinLongEdge: 1280})
	tests := []struct {
		name                 string
		srcW, srcH           uint16
		wantW, wantH         uint16
		wantFilterHasScaleTo string
	}{
		{"가로 소스", 640, 360, 1280, 720, "scale=1280:720"},
		{"세로 소스", 270, 480, 720, 1280, "scale=720:1280"},
		{"세로 3:4", 480, 640, 960, 1280, "scale=960:1280"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			width, height, filter, err := transcoder.decoderOutput(tc.srcW, tc.srcH)
			if err != nil {
				t.Fatalf("decoderOutput: %v", err)
			}
			if width != tc.wantW || height != tc.wantH {
				t.Fatalf("치수 = %dx%d, want %dx%d", width, height, tc.wantW, tc.wantH)
			}
			if !strings.Contains(filter, tc.wantFilterHasScaleTo) {
				t.Fatalf("필터에 %q가 없다: %s", tc.wantFilterHasScaleTo, filter)
			}
			// pad가 빠지면 비율이 어긋날 때 잘려나간다.
			if !strings.Contains(filter, "force_original_aspect_ratio=decrease") ||
				!strings.Contains(filter, "pad=") {
				t.Fatalf("필터에 decrease+pad가 없다: %s", filter)
			}
		})
	}
}

// TestDecoderSpawnArgumentsPinOff는 핀이 꺼졌을 때 두 디코더의 ffmpeg 인자에
// -vf가 아예 붙지 않는지 확인한다. decoderOutput 단위 테스트만으로는 스폰까지
// 전달되는지를 못 잡는다.
func TestDecoderSpawnArgumentsPinOff(t *testing.T) {
	h264 := captureFFmpegArgs(t,
		TranscoderOptions{VideoCodec: VideoCodecH264, WireFormat: config.WireFormatJPEG},
		func(transcoder *FFmpegTranscoder) error {
			process, err := transcoder.startH264Decoder(context.Background(), "")
			if err == nil {
				process.close()
			}
			return err
		})
	vp8 := captureFFmpegArgs(t,
		TranscoderOptions{WireFormat: config.WireFormatJPEG},
		func(transcoder *FFmpegTranscoder) error {
			process, err := transcoder.startDecoder(context.Background(), 640, 360, "")
			if err == nil {
				process.close()
			}
			return err
		})
	for name, arguments := range map[string][]string{"H.264": h264, "VP8": vp8} {
		for _, argument := range arguments {
			if argument == "-vf" {
				t.Errorf("%s: 핀이 꺼졌는데 -vf가 붙었다:\n  %s", name, strings.Join(arguments, " "))
			}
		}
	}
}

// TestDecoderSpawnArgumentsPinOn은 필터가 스폰 인자까지 전달되는지 본다.
func TestDecoderSpawnArgumentsPinOn(t *testing.T) {
	const filter = "scale=720:1280:force_original_aspect_ratio=decrease,pad=720:1280:-1:-1"
	h264 := captureFFmpegArgs(t,
		TranscoderOptions{VideoCodec: VideoCodecH264, WireFormat: config.WireFormatJPEG, PinLongEdge: 1280},
		func(transcoder *FFmpegTranscoder) error {
			process, err := transcoder.startH264Decoder(context.Background(), filter)
			if err == nil {
				process.close()
			}
			return err
		})
	vp8 := captureFFmpegArgs(t,
		TranscoderOptions{WireFormat: config.WireFormatJPEG, PinLongEdge: 1280},
		func(transcoder *FFmpegTranscoder) error {
			process, err := transcoder.startDecoder(context.Background(), 270, 480, filter)
			if err == nil {
				process.close()
			}
			return err
		})
	for name, arguments := range map[string][]string{"H.264": h264, "VP8": vp8} {
		assertArgPair(t, arguments, "-vf", filter)
		// 입력 기술자와 출력 코덱 계약은 그대로여야 한다.
		if !strings.Contains(strings.Join(arguments, " "), "-probesize 32") {
			t.Errorf("%s: 저지연 인자가 사라졌다", name)
		}
	}
}
