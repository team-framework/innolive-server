package media

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"inno-live-server/internal/config"
)

// captureFFmpegArgs는 실제 ffmpeg를 띄우지 않고 조립된 인자만 가로챈다.
// spawn 함수가 프로세스를 만들어 반환해야 하므로 `cat`으로 대체한다.
func captureFFmpegArgs(t *testing.T, options TranscoderOptions, spawn func(*FFmpegTranscoder) error) []string {
	t.Helper()
	var captured []string
	transcoder := NewFFmpegTranscoder("ffmpeg", testLogger(), nil, options)
	transcoder.newCommand = func(ctx context.Context, _ string, arguments ...string) *exec.Cmd {
		captured = append([]string(nil), arguments...)
		return exec.CommandContext(ctx, "cat")
	}
	if err := spawn(transcoder); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if len(captured) == 0 {
		t.Fatal("ffmpeg 인자를 가로채지 못했다")
	}
	return captured
}

// assertArgPair는 `-flag value` 쌍이 인자 목록에 있는지 확인한다.
func assertArgPair(t *testing.T, arguments []string, flag, value string) {
	t.Helper()
	for i := 0; i+1 < len(arguments); i++ {
		if arguments[i] == flag && arguments[i+1] == value {
			return
		}
	}
	t.Errorf("%s %s 가 없다:\n  %s", flag, value, strings.Join(arguments, " "))
}

// TestH264DecoderUsesLowLatencyArgs는 H.264 디코더가 VP8 디코더와 같은
// 저지연 인자를 쓰는지 고정한다. 이 인자들이 빠지면 ffmpeg가 기본 probe
// 예산(5MB / 5초)을 채울 때까지 출력을 시작하지 않아 스폰부터 첫 프레임까지
// 1.7초가 걸린다 — 세션이 시작될 때마다 그대로 붙는 지연이다(#130).
func TestH264DecoderUsesLowLatencyArgs(t *testing.T) {
	for _, wire := range []config.WireFormat{config.WireFormatJPEG, config.WireFormatRaw} {
		t.Run(string(wire), func(t *testing.T) {
			arguments := captureFFmpegArgs(t,
				TranscoderOptions{VideoCodec: VideoCodecH264, WireFormat: wire},
				func(transcoder *FFmpegTranscoder) error {
					process, err := transcoder.startH264Decoder(context.Background())
					if err == nil {
						process.close()
					}
					return err
				})

			// VP8 startDecoder와 동일해야 하는 것들.
			assertArgPair(t, arguments, "-probesize", "32")
			assertArgPair(t, arguments, "-analyzeduration", "0")
			assertArgPair(t, arguments, "-fpsprobesize", "0")
			assertArgPair(t, arguments, "-threads", "1")
			assertArgPair(t, arguments, "-blocksize", "1024")
			// 출력 코덱 계약은 그대로여야 한다.
			assertArgPair(t, arguments, "-f", "h264")
			assertArgPair(t, arguments, "-flush_packets", "1")
			if wire == config.WireFormatJPEG {
				assertArgPair(t, arguments, "-q:v", "3")
				assertArgPair(t, arguments, "-pix_fmt", "yuvj420p")
			} else {
				assertArgPair(t, arguments, "-pix_fmt", "yuv420p")
			}
		})
	}
}

// TestH264EncoderUsesLowLatencyArgs는 인코더 쪽 동일 항목을 고정한다.
// 스레드는 VP8 startEncoder와 마찬가지로 자원 거버너를 따르므로,
// 하드코딩이 아니라 EncoderThreads가 반영되는지를 본다.
func TestH264EncoderUsesLowLatencyArgs(t *testing.T) {
	arguments := captureFFmpegArgs(t,
		TranscoderOptions{VideoCodec: VideoCodecH264, WireFormat: config.WireFormatJPEG},
		func(transcoder *FFmpegTranscoder) error {
			process, err := transcoder.startH264Encoder(context.Background(), 1280, 720)
			if err == nil {
				process.close()
			}
			return err
		})

	assertArgPair(t, arguments, "-probesize", "32")
	assertArgPair(t, arguments, "-analyzeduration", "0")
	assertArgPair(t, arguments, "-fpsprobesize", "0")
	assertArgPair(t, arguments, "-blocksize", "1024")
	// 인코딩 계약은 그대로여야 한다.
	assertArgPair(t, arguments, "-c:v", "libx264")
	assertArgPair(t, arguments, "-profile:v", "baseline")
	assertArgPair(t, arguments, "-tune", "zerolatency")

	// 스레드는 하드코딩이 아니라 자원 거버너를 따른다.
	for _, threads := range []int{0, 1, 4} {
		captured := captureFFmpegArgs(t,
			TranscoderOptions{VideoCodec: VideoCodecH264, WireFormat: config.WireFormatJPEG, EncoderThreads: threads},
			func(transcoder *FFmpegTranscoder) error {
				process, err := transcoder.startH264Encoder(context.Background(), 640, 360)
				if err == nil {
					process.close()
				}
				return err
			})
		joined := strings.Join(captured, " ")
		if threads == 0 {
			if strings.Contains(joined, "-threads") {
				t.Errorf("EncoderThreads=0인데 -threads가 붙었다:\n  %s", joined)
			}
			continue
		}
		assertArgPair(t, captured, "-threads", map[int]string{1: "1", 4: "4"}[threads])
	}
}
