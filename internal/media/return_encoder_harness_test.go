//go:build egress_harness

// 반환 인코더 해상도별 설정(#478)의 실 ffmpeg 검증. 720p·FHD 처리 프레임을
// VP8·H.264 반환 인코더에 넣고 출력 해상도·H.264 level·bitrate를 확인한다.
// H.264는 preset을 바꿔도(#487) 협상한 Constrained Baseline 규격인지 함께 본다.
//
//	go test -tags egress_harness -run TestReturnEncoderResolutionProfile ./internal/media -v
package media

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

// patternFrames는 움직이는 테스트 패턴(testsrc2)을 raw yuv420p 프레임으로 만든다.
// 0으로 채운 프레임은 bitrate를 거의 쓰지 않아 rate control을 검증할 수 없다.
func patternFrames(t *testing.T, count int, width, height uint16) []frame {
	t.Helper()
	output, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=s=%dx%d:r=30,noise=alls=6:allf=t", width, height),
		"-frames:v", fmt.Sprint(count), "-pix_fmt", "yuv420p", "-f", "rawvideo", "pipe:1").Output()
	if err != nil {
		t.Fatalf("generate pattern frames: %v", err)
	}
	size := rawFrameSize(width, height)
	if len(output) != size*count {
		t.Fatalf("pattern bytes=%d, want %d", len(output), size*count)
	}
	frames := make([]frame, count)
	for i := range frames {
		frames[i] = frame{data: output[i*size : (i+1)*size], width: width, height: height, timestamp: uint32(i * 3000)}
	}
	return frames
}

func TestReturnEncoderResolutionProfile(t *testing.T) {
	requireTool(t, "ffmpeg")
	const seconds = 3
	for _, tc := range []struct {
		width, height uint16
		bitsPerSecond int
		levelIDC      byte
	}{
		{720, 1280, 2_000_000, 31},
		{1080, 1920, 4_000_000, 40},
	} {
		frames := patternFrames(t, seconds*30, tc.width, tc.height)
		for _, codec := range []VideoCodec{VideoCodecVP8, VideoCodecH264} {
			t.Run(fmt.Sprintf("%s/%dx%d", codec, tc.width, tc.height), func(t *testing.T) {
				transcoder := NewFFmpegTranscoder("ffmpeg", testLogger(), metrics.New(), TranscoderOptions{
					VideoCodec: codec, WireFormat: config.WireFormatRaw, EncoderThreads: 1,
				})
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

				count, total, largestKey := 0, 0, 0
				var width, height uint16
				var levelIDC, profileIDC, constraints byte
				for item := range output {
					count++
					total += len(item.data)
					if codec == VideoCodecVP8 {
						if w, h, err := vp8KeyframeDimensions(item.data); err == nil {
							width, height = w, h
							largestKey = max(largestKey, len(item.data))
						}
						continue
					}
					for _, nalu := range splitAnnexBNALUs(item.data) {
						if len(nalu) > 3 && nalu[0]&0x1f == 7 {
							profileIDC, constraints, levelIDC = nalu[1], nalu[2], nalu[3]
							if w, h, err := h264SPSDimensions(nalu); err == nil {
								width, height = w, h
							}
							largestKey = max(largestKey, len(item.data))
						}
					}
				}
				if count != len(frames) {
					t.Fatalf("encoded %d frames, want %d", count, len(frames))
				}
				if width != tc.width || height != tc.height {
					t.Fatalf("encoded resolution=%dx%d, want %dx%d", width, height, tc.width, tc.height)
				}
				if codec == VideoCodecH264 && levelIDC != tc.levelIDC {
					t.Fatalf("H.264 level_idc=%d, want %d", levelIDC, tc.levelIDC)
				}
				// Constrained Baseline: profile_idc 66, constraint_set1_flag 1(profile-level-id 42e0xx).
				if codec == VideoCodecH264 && (profileIDC != 66 || constraints&0x40 == 0) {
					t.Fatalf("H.264 profile_idc=%d constraints=%#x, want Constrained Baseline", profileIDC, constraints)
				}
				bitsPerSecond := total * 8 / seconds
				if bitsPerSecond < tc.bitsPerSecond*6/10 || bitsPerSecond > tc.bitsPerSecond*13/10 {
					t.Fatalf("bitrate=%d bit/s, want about %d", bitsPerSecond, tc.bitsPerSecond)
				}
				t.Logf("%s %dx%d: %d kbit/s, largest keyframe %d bytes", codec, width, height, bitsPerSecond/1000, largestKey)
			})
		}
	}
}
