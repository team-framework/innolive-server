//go:build egress_harness

// 디코더 취소 종료 검증(#497). 실 ffmpeg 디코더가 출력을 다 내기 전에 파이프라인이
// 취소되는 경우를 재현하므로 egress_harness 빌드 태그 뒤에 두고, 도구가 없으면
// 건너뛴다.
//
//	go test -tags egress_harness -run TestDecodeStreamCancelDoesNotWaitShutdownGrace ./internal/media -v
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

// TestDecodeStreamCancelDoesNotWaitShutdownGrace는 #283 재시작처럼 디코더 출력이
// 남은 채 파이프라인이 취소돼도 DecodeStream이 grace(2초)를 기다리지 않고
// 돌아오는지 본다. 하류가 출력을 받지 않아 디코더가 raw 720p 프레임(1.38MB,
// 파이프 버퍼보다 큼)을 쓰다 멈춘 상태에서 취소한다.
func TestDecodeStreamCancelDoesNotWaitShutdownGrace(t *testing.T) {
	requireTool(t, "ffmpeg")

	for _, test := range []struct {
		name   string
		codec  VideoCodec
		frames func(t *testing.T) [][]byte
	}{
		{name: "VP8", codec: VideoCodecVP8, frames: vp8TeardownFrames},
		{name: "H.264", codec: VideoCodecH264, frames: h264TeardownFrames},
	} {
		t.Run(test.name, func(t *testing.T) {
			frames := test.frames(t)
			transcoder := NewFFmpegTranscoder("ffmpeg", testLogger(), metrics.New(),
				TranscoderOptions{VideoCodec: test.codec, WireFormat: config.WireFormatRaw, PinLongEdge: 1280})

			// 입력은 열어 둔 채(실시간 트랙) 프레임을 넣고, 출력은 받지 않는다.
			input := make(chan frame, len(frames))
			for index, data := range frames {
				input <- frame{data: data, timestamp: uint32(index * 3000), stageAt: time.Now()}
			}
			output := make(chan frame)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			returned := make(chan time.Time, 1)
			go func() {
				_ = transcoder.DecodeStream(ctx, input, output, nil)
				returned <- time.Now()
			}()
			// 디코더가 뜨고 첫 출력을 넘기려다 막힐 시간을 준다.
			time.Sleep(500 * time.Millisecond)

			cancelled := time.Now()
			cancel()
			var elapsed time.Duration
			select {
			case at := <-returned:
				elapsed = at.Sub(cancelled)
			case <-time.After(10 * time.Second):
				t.Fatal("DecodeStream did not return after cancellation")
			}
			if elapsed >= ffmpegShutdownGrace/2 {
				t.Fatalf("DecodeStream returned %v after cancel, want well under the %v shutdown grace", elapsed, ffmpegShutdownGrace)
			}
			t.Logf("DecodeStream returned %v after cancel", elapsed)
		})
	}
}

// vp8TeardownFrames는 키프레임 하나로 시작하는 1280x720 VP8 프레임 30개를 만든다.
func vp8TeardownFrames(t *testing.T) [][]byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "teardown.ivf")
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30",
		"-frames:v", "30", "-c:v", "libvpx", "-deadline", "realtime", "-cpu-used", "8", "-b:v", "2M",
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

// h264TeardownFrames는 SPS·PPS를 실은 IDR로 시작하는 1280x720 H.264 액세스
// 유닛 30개를 만든다.
func h264TeardownFrames(t *testing.T) [][]byte {
	t.Helper()
	stream, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30",
		"-frames:v", "30", "-pix_fmt", "yuv420p", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-profile:v", "baseline", "-bf", "0", "-x264-params", "repeat-headers=1:aud=1:annexb=1",
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
