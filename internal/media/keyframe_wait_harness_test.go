//go:build egress_harness

// 키프레임 대기 폐기·요청 검증(#493). 실 ffmpeg로 만든 스트림을 GOP 중간부터
// 넣으므로 egress_harness 빌드 태그 뒤에 두고, 도구가 없으면 건너뛴다.
//
//	go test -tags egress_harness -run TestDecodeStreamMidGOP ./internal/media -v
package media

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

// TestDecodeStreamMidGOPCountsDiscardsAndDecodesFromKeyframe는 #283 재시작처럼
// 파이프라인이 GOP 중간에서 뜬 경우를 실 디코더로 재현한다. 첫 키프레임 앞의
// 인터프레임은 모두 폐기 카운터와 대기 종료 로그에 남고, 송신자에게 키프레임을
// 한 번 요청하며, 키프레임부터는 모든 프레임이 디코드된다.
func TestDecodeStreamMidGOPCountsDiscardsAndDecodesFromKeyframe(t *testing.T) {
	requireTool(t, "ffmpeg")

	for _, test := range []struct {
		name       string
		codec      VideoCodec
		frames     func(t *testing.T) [][]byte
		isKeyframe func([]byte) bool
	}{
		{
			name:   "VP8",
			codec:  VideoCodecVP8,
			frames: vp8MidGOPFrames,
			isKeyframe: func(data []byte) bool {
				_, _, err := vp8KeyframeDimensions(data)
				return err == nil
			},
		},
		{
			name:   "H.264",
			codec:  VideoCodecH264,
			frames: h264MidGOPFrames,
			isKeyframe: func(data []byte) bool {
				for _, nalu := range splitAnnexBNALUs(data) {
					if len(nalu) > 0 && nalu[0]&0x1f == 5 {
						return true
					}
				}
				return false
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 첫 키프레임을 빼고 GOP 중간부터 넣는다.
			frames := test.frames(t)[1:]
			waiting := 0
			for waiting < len(frames) && !test.isKeyframe(frames[waiting]) {
				waiting++
			}
			if waiting == 0 || waiting == len(frames) {
				t.Fatalf("fixture has %d frames before the next keyframe out of %d", waiting, len(frames))
			}

			registry := metrics.New()
			var logs capturedLog
			transcoder := NewFFmpegTranscoder("ffmpeg", logs.logger(), registry,
				TranscoderOptions{VideoCodec: test.codec, WireFormat: config.WireFormatJPEG})
			var requests int
			keyframes := newKeyframeWait(registry, config.PrivacyModeReal, func() { requests++ })

			input := make(chan frame, len(frames))
			for index, data := range frames {
				input <- frame{data: data, timestamp: uint32(index * 3000)}
			}
			close(input)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			output := make(chan frame, len(frames))
			if err := transcoder.DecodeStream(ctx, input, output, keyframes); err != nil {
				t.Fatalf("DecodeStream() error = %v", err)
			}
			close(output)
			decoded := 0
			for item := range output {
				if len(item.data) == 0 {
					t.Fatalf("decoded frame %d is empty", decoded)
				}
				decoded++
			}

			if got := counterValue(t, registry, keyframeWaitMetric); got != float64(waiting) {
				t.Fatalf("keyframe wait discards = %v, want %d", got, waiting)
			}
			// 폐기가 500ms 안에 몰려 있으므로 요청은 첫 폐기의 한 번이다.
			if requests != 1 {
				t.Fatalf("keyframe requests = %d, want 1", requests)
			}
			if want := len(frames) - waiting; decoded != want {
				t.Fatalf("decoded frames = %d, want %d from the first keyframe on", decoded, want)
			}
			records := logs.records(t, "decoder keyframe wait ended")
			if len(records) != 1 || records[0]["keyframe_received"] != true ||
				records[0]["discarded_frames"] != float64(waiting) || records[0]["keyframe_requests"] != float64(1) {
				t.Fatalf("keyframe wait logs = %v, want one with keyframe_received=true discarded_frames=%d keyframe_requests=1", records, waiting)
			}
			t.Logf("fed %d frames, discarded %d before the keyframe, decoded %d", len(frames), waiting, decoded)
		})
	}
}

// vp8MidGOPFrames는 15프레임마다 키프레임이 있는 VP8 프레임 30개를 만든다.
func vp8MidGOPFrames(t *testing.T) [][]byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gop.ivf")
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x180:rate=30",
		"-frames:v", "30", "-c:v", "libvpx", "-deadline", "realtime", "-g", "15", "-keyint_min", "15",
		path)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate VP8 fixture: %v\n%s", err, out)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := io.ReadFull(file, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	var frames [][]byte
	for {
		data, _, err := readIVFFrame(file)
		if err != nil {
			break
		}
		frames = append(frames, data)
	}
	return frames
}

// h264MidGOPFrames는 15프레임마다 SPS·PPS를 실은 IDR이 있는 H.264 액세스 유닛
// 30개를 만든다.
func h264MidGOPFrames(t *testing.T) [][]byte {
	t.Helper()
	stream, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x180:rate=30",
		"-frames:v", "30", "-pix_fmt", "yuv420p", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-profile:v", "baseline", "-g", "15", "-bf", "0", "-x264-params", "scenecut=0:repeat-headers=1:aud=1:annexb=1",
		"-f", "h264", "pipe:1",
	).Output()
	if err != nil {
		t.Fatalf("generate H.264 fixture: %v", err)
	}
	reader := h264AccessUnitReader{reader: bufio.NewReader(bytes.NewReader(stream))}
	var frames [][]byte
	for {
		unit, err := reader.Read()
		if err != nil {
			break
		}
		frames = append(frames, unit)
	}
	return frames
}
