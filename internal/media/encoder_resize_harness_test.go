//go:build egress_harness

// 인코더 재기동(#449)의 실 ffmpeg 검증. 처리 프레임 크기가 도중에 바뀌어도
// 넣은 프레임마다 인코딩 결과가 하나씩 나와야 한다.
//
//	go test -tags egress_harness -run TestEncoderRestarts ./internal/media -v
package media

import (
	"context"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

func rawFrames(count int, width, height uint16) []frame {
	frames := make([]frame, count)
	for i := range frames {
		frames[i] = frame{data: make([]byte, rawFrameSize(width, height)), width: width, height: height}
	}
	return frames
}

func TestEncoderRestartsOnFrameResize(t *testing.T) {
	requireTool(t, "ffmpeg")
	for _, codec := range []VideoCodec{VideoCodecVP8, VideoCodecH264} {
		t.Run(string(codec), func(t *testing.T) {
			transcoder := NewFFmpegTranscoder("ffmpeg", testLogger(), metrics.New(), TranscoderOptions{
				VideoCodec: codec, WireFormat: config.WireFormatRaw,
			})
			frames := append(rawFrames(10, 720, 1280), rawFrames(10, 1280, 720)...)
			input := make(chan frame, len(frames))
			for _, item := range frames {
				input <- item
			}
			close(input)
			output := make(chan frame, len(frames))
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := transcoder.EncodeStream(ctx, input, output); err != nil {
				t.Fatalf("EncodeStream() error = %v", err)
			}
			close(output)
			encoded := 0
			for item := range output {
				if len(item.data) == 0 {
					t.Fatalf("frame %d is empty", encoded)
				}
				encoded++
			}
			if encoded != len(frames) {
				t.Fatalf("encoded %d frames, want %d", encoded, len(frames))
			}
		})
	}
}
