package media

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

const keyframeWaitMetric = `innolive_frame_keyframe_wait_discarded_total{mode="real"}`

// steppedClock은 부를 때마다 step만큼 흐르는 시계다. 0부터 시작한다.
func steppedClock(step time.Duration) func() time.Time {
	now := time.Unix(0, 0)
	return func() time.Time {
		current := now
		now = now.Add(step)
		return current
	}
}

// TestKeyframeWaitRequestsOnFirstDiscardThenThrottles는 #493의 요청 간격을 고정한다.
// 첫 폐기에서 바로 요청하고, 계속 기다리면 손실 복구와 같은 500ms 간격으로만
// 다시 요청한다.
func TestKeyframeWaitRequestsOnFirstDiscardThenThrottles(t *testing.T) {
	registry := metrics.New()
	var requests int
	keyframes := newKeyframeWait(registry, config.PrivacyModeReal, func() { requests++ })
	if keyframes.minInterval != keyframeRequestMinInterval {
		t.Fatalf("minInterval = %v, want %v", keyframes.minInterval, keyframeRequestMinInterval)
	}
	start := time.Unix(0, 0)
	for _, step := range []struct {
		at   time.Duration
		want int
	}{
		{0, 1},
		{100 * time.Millisecond, 1},
		{499 * time.Millisecond, 1},
		{500 * time.Millisecond, 2},
		{900 * time.Millisecond, 2},
		{1000 * time.Millisecond, 3},
	} {
		keyframes.now = func() time.Time { return start.Add(step.at) }
		keyframes.discard()
		if requests != step.want {
			t.Fatalf("at %v keyframe requests = %d, want %d", step.at, requests, step.want)
		}
	}
	if got := counterValue(t, registry, keyframeWaitMetric); got != 6 {
		t.Fatalf("keyframe wait discards = %v, want 6", got)
	}
}

// TestKeyframeWaitWithoutRequesterStillCounts: 피드백 채널이 없는 호출자도 폐기는
// 남고, nil 대기자는 아무것도 하지 않는다.
func TestKeyframeWaitWithoutRequesterStillCounts(t *testing.T) {
	var missing *keyframeWait
	missing.discard()

	registry := metrics.New()
	keyframes := newKeyframeWait(registry, config.PrivacyModeReal, nil)
	keyframes.discard()
	keyframes.discard()
	if got := counterValue(t, registry, keyframeWaitMetric); got != 2 {
		t.Fatalf("keyframe wait discards = %v, want 2", got)
	}
}

// sinkDecoder는 FFmpeg 대신 stdin을 끝까지 읽고 아무것도 내지 않는 프로세스를
// 디코더로 띄운다. 입력이 닫혀 stdin이 닫혀야 끝나므로, DecodeStream이 돌아올
// 때는 키프레임 뒤 입력까지 모두 디코더에 넘긴 뒤다.
func sinkDecoder(transcoder *FFmpegTranscoder) {
	transcoder.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "cat > /dev/null")
	}
}

// annexB는 NAL 유닛들을 4바이트 시작 코드로 잇는다.
func annexB(nalus ...[]byte) []byte {
	var data []byte
	for _, nalu := range nalus {
		data = append(data, h264AnnexBStartCode...)
		data = append(data, nalu...)
	}
	return data
}

// 64x48 baseline 스트림에서 뽑은 SPS·PPS다(libx264).
var (
	testH264SPS = []byte{0x67, 0x42, 0xc0, 0x0a, 0xda, 0x11, 0xec, 0x04, 0x40, 0x00, 0x00, 0x03, 0x00, 0x40, 0x00, 0x00, 0x0f, 0x03, 0xc4, 0x89, 0xa8}
	testH264PPS = []byte{0x68, 0xce, 0x0f, 0xc8}
)

// TestDecodeStreamCountsAndRequestsKeyframeUntilFirstKeyframe는 #493의 수용
// 기준이다. 파이프라인이 GOP 중간에서 뜨면(#283 재시작) 디코더는 첫 키프레임까지
// 프레임을 버린다. 그 폐기는 카운터에 남고, 기다리는 동안 송신자에게 키프레임을
// 요청해야 한다. 첫 키프레임 뒤의 인터프레임은 디코더로 가며 세지도 요청하지도
// 않는다.
func TestDecodeStreamCountsAndRequestsKeyframeUntilFirstKeyframe(t *testing.T) {
	for _, test := range []struct {
		name     string
		codec    VideoCodec
		inter    []byte
		keyframe []byte
	}{
		{
			name:     "VP8",
			codec:    VideoCodecVP8,
			inter:    []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			keyframe: []byte{0x00, 0x00, 0x00, 0x9d, 0x01, 0x2a, 0x40, 0x00, 0x30, 0x00},
		},
		{
			name:     "H.264",
			codec:    VideoCodecH264,
			inter:    annexB([]byte{0x41, 0x9a, 0x00, 0x10}),
			keyframe: annexB(testH264SPS, testH264PPS, []byte{0x65, 0x88, 0x84, 0x00}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := metrics.New()
			transcoder := NewFFmpegTranscoder("ffmpeg", testLogger(), registry, TranscoderOptions{VideoCodec: test.codec})
			sinkDecoder(transcoder)
			var requests int
			keyframes := newKeyframeWait(registry, config.PrivacyModeReal, func() { requests++ })
			// 프레임 하나에 200ms씩 기다린다고 본다. 폐기는 0·200·400·600ms에 일어나므로
			// 요청은 0ms와 600ms 두 번이다.
			keyframes.now = steppedClock(200 * time.Millisecond)

			const waiting, after = 4, 3
			input := make(chan frame, waiting+1+after)
			for index := 0; index < waiting; index++ {
				input <- frame{data: test.inter, timestamp: uint32(index * 3000)}
			}
			input <- frame{data: test.keyframe, timestamp: uint32(waiting * 3000)}
			for index := 0; index < after; index++ {
				input <- frame{data: test.inter, timestamp: uint32((waiting + 1 + index) * 3000)}
			}
			close(input)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// 가짜 디코더는 출력이 없으므로 읽기 오류로 끝난다. 여기서 보는 것은
			// 디코더를 띄우기 전까지의 폐기와 요청뿐이다.
			_ = transcoder.DecodeStream(ctx, input, make(chan frame, waiting+1+after), keyframes)
			if ctx.Err() != nil {
				t.Fatal("DecodeStream() did not finish")
			}

			if remaining := len(input); remaining != 0 {
				t.Fatalf("%d frames after the keyframe were never read", remaining)
			}
			if got := counterValue(t, registry, keyframeWaitMetric); got != waiting {
				t.Fatalf("keyframe wait discards = %v, want %d", got, waiting)
			}
			if requests != 2 {
				t.Fatalf("keyframe requests = %d, want 2 (first discard, then after 500ms)", requests)
			}
		})
	}
}

// TestDecodeStreamStartingOnKeyframeRequestsNothing: 키프레임으로 시작하는 평소
// 경로는 아무것도 버리지 않고 송신자에게 요청도 보내지 않는다.
func TestDecodeStreamStartingOnKeyframeRequestsNothing(t *testing.T) {
	registry := metrics.New()
	transcoder := NewFFmpegTranscoder("ffmpeg", testLogger(), registry, TranscoderOptions{})
	sinkDecoder(transcoder)
	var requests int
	keyframes := newKeyframeWait(registry, config.PrivacyModeReal, func() { requests++ })

	input := make(chan frame, 3)
	input <- frame{data: []byte{0x00, 0x00, 0x00, 0x9d, 0x01, 0x2a, 0x40, 0x00, 0x30, 0x00}}
	input <- frame{data: []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}}
	input <- frame{data: []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}}
	close(input)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = transcoder.DecodeStream(ctx, input, make(chan frame, 3), keyframes)
	if ctx.Err() != nil {
		t.Fatal("DecodeStream() did not finish")
	}

	if got := counterValue(t, registry, keyframeWaitMetric); got != 0 {
		t.Fatalf("keyframe wait discards = %v, want 0", got)
	}
	if requests != 0 {
		t.Fatalf("keyframe requests = %d, want 0", requests)
	}
}
