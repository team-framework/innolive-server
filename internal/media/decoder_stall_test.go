package media

import (
	"context"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"inno-live-server/internal/metrics"
)

// 패킷 손실 뒤 FFmpeg가 입력을 받기만 하고 프레임을 내지 않으면, 디코더는 입력
// 하나에 출력 하나를 기다리다 메타데이터 큐(64)가 차서 쓰기와 읽기가 서로를
// 기다린다(P0-4). 감시가 이를 알아채고 다음 키프레임부터 디코더를 다시 띄워야
// 영상이 멈춘 채로 남지 않는다.
func TestDecoderRestartsWhenFFmpegStopsProducingFrames(t *testing.T) {
	for _, codec := range keyframeWaitCodecs {
		t.Run(codec.name, func(t *testing.T) {
			testDecoderRestartsAfterStall(t, codec.codec, codec.keyframe)
		})
	}
}

func testDecoderRestartsAfterStall(t *testing.T, codec VideoCodec, keyframe []byte) {
	transcoder := NewFFmpegTranscoder("ffmpeg", testLogger(), metrics.New(), TranscoderOptions{VideoCodec: codec})
	var spawned atomic.Int32
	transcoder.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		spawned.Add(1)
		// 입력은 받되 아무것도 출력하지 않는 디코더.
		return exec.CommandContext(ctx, "sh", "-c", "cat > /dev/null")
	}
	input := make(chan frame, 128)
	for range 100 {
		input <- frame{data: keyframe, stageAt: time.Now()}
	}
	output := make(chan frame, 128)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- transcoder.DecodeStream(ctx, input, output, nil) }()

	deadline := time.Now().Add(8 * time.Second)
	for spawned.Load() < 2 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("decoders spawned = %d, want a restart after the stall", spawned.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("DecodeStream did not return after cancel")
	}
}
