package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"testing"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

func TestRealProcessorBlackoutLatchesAndStopsCallingAI(t *testing.T) {
	calls := 0
	ai := &fakeAIStream{process: func([]byte, int64) (*aiv1.ProcessedVideoChunk, error) {
		calls++
		return nil, errors.New("ai unavailable")
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyBlackoutLatch, 0)
	if err != nil {
		t.Fatal(err)
	}

	first, err := processor.Process(context.Background(), []byte("not-a-jpeg"), 1, 64, 48)
	if err != nil {
		t.Fatalf("Process() after AI failure error = %v, want blackout frame", err)
	}
	if !isJPEG(first) {
		t.Fatalf("blackout frame is not a JPEG image: %v...", first[:min(8, len(first))])
	}
	decoded, err := jpeg.DecodeConfig(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Width != 64 || decoded.Height != 48 {
		t.Fatalf("blackout dimensions = %dx%d, want 64x48", decoded.Width, decoded.Height)
	}
	if !processor.FallbackActive() {
		t.Fatal("FallbackActive() = false after AI failure, want latched")
	}
	if calls != 1 {
		t.Fatalf("AI calls = %d, want 1", calls)
	}

	second, err := processor.Process(context.Background(), []byte("next-frame"), 2, 64, 48)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("latched session should serve the cached blackout frame")
	}
	if calls != 1 {
		t.Fatalf("AI calls after latch = %d, want still 1 (latch must stop AI calls)", calls)
	}
}

func TestRealProcessorFreezePolicyPropagatesError(t *testing.T) {
	calls := 0
	ai := &fakeAIStream{process: func([]byte, int64) (*aiv1.ProcessedVideoChunk, error) {
		calls++
		return nil, errors.New("ai unavailable")
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyFreeze, 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := processor.Process(context.Background(), []byte("frame"), 1, 64, 48); err == nil {
		t.Fatal("freeze policy should propagate the AI error")
	}
	if processor.FallbackActive() {
		t.Fatal("freeze policy must not latch the blackout")
	}
	if _, err := processor.Process(context.Background(), []byte("frame"), 2, 64, 48); err == nil {
		t.Fatal("freeze policy should keep failing while AI fails")
	}
	if calls != 2 {
		t.Fatalf("AI calls = %d, want 2 (freeze keeps retrying)", calls)
	}
}

func TestRealProcessorToleratesTransientTimeoutsThenLatches(t *testing.T) {
	calls := 0
	ai := &fakeAIStream{process: func([]byte, int64) (*aiv1.ProcessedVideoChunk, error) {
		calls++
		return nil, context.DeadlineExceeded
	}}
	const threshold = 2
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyBlackoutLatch, threshold)
	if err != nil {
		t.Fatal(err)
	}

	// 처음 `threshold`번의 연속 타임아웃은 허용한다. 블랙아웃 프레임을 내보내지만
	// 세션을 잠그지 않고 AI를 계속 다시 시도한다.
	for i := 1; i <= threshold; i++ {
		frame, err := processor.Process(context.Background(), []byte("frame"), int64(i), 64, 48)
		if err != nil {
			t.Fatalf("timeout %d: Process() error = %v, want a blackout frame", i, err)
		}
		if !isJPEG(frame) {
			t.Fatalf("timeout %d: served frame is not a blackout JPEG", i)
		}
		if processor.FallbackActive() {
			t.Fatalf("timeout %d: latched early (threshold=%d not yet exceeded)", i, threshold)
		}
		if calls != i {
			t.Fatalf("timeout %d: AI calls = %d, want %d (AI must keep being retried)", i, calls, i)
		}
	}

	// 한계를 넘으면 영구히 잠근다.
	if _, err := processor.Process(context.Background(), []byte("frame"), 99, 64, 48); err != nil {
		t.Fatalf("Process() at latch error = %v", err)
	}
	if !processor.FallbackActive() {
		t.Fatal("session did not latch after exceeding the timeout threshold")
	}
	if calls != threshold+1 {
		t.Fatalf("AI calls at latch = %d, want %d", calls, threshold+1)
	}

	// 잠근 뒤에는 AI를 더 부르지 않는다.
	if _, err := processor.Process(context.Background(), []byte("frame"), 100, 64, 48); err != nil {
		t.Fatal(err)
	}
	if calls != threshold+1 {
		t.Fatalf("AI calls after latch = %d, want still %d", calls, threshold+1)
	}
}

func TestRealProcessorNonTimeoutLatchesImmediatelyDespiteThreshold(t *testing.T) {
	calls := 0
	ai := &fakeAIStream{process: func([]byte, int64) (*aiv1.ProcessedVideoChunk, error) {
		calls++
		return nil, errors.New("status=\"failed\"")
	}}
	// 한계를 넉넉히 줘도 타임아웃이 아닌(확정적) 실패는 보호하지 않는다.
	// 이전처럼 첫 실패에서 바로 잠근다.
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyBlackoutLatch, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := processor.Process(context.Background(), []byte("frame"), 1, 64, 48); err != nil {
		t.Fatal(err)
	}
	if !processor.FallbackActive() {
		t.Fatal("non-timeout failure should latch immediately regardless of the timeout threshold")
	}
	if calls != 1 {
		t.Fatalf("AI calls = %d, want 1", calls)
	}
}

func TestRealProcessorTimeoutStreakResetsOnSuccess(t *testing.T) {
	calls := 0
	failing := true
	ai := &fakeAIStream{process: func(_ []byte, ts int64) (*aiv1.ProcessedVideoChunk, error) {
		calls++
		if failing {
			return nil, context.DeadlineExceeded
		}
		return &aiv1.ProcessedVideoChunk{Data: []byte("ok"), Timestamp: ts, StatusMessage: "success"}, nil
	}}
	const threshold = 2
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyBlackoutLatch, threshold)
	if err != nil {
		t.Fatal(err)
	}

	// 타임아웃 두 번(한계 안) 뒤 성공하면 연속 횟수를 초기화해야 한다.
	for i := 1; i <= threshold; i++ {
		if _, err := processor.Process(context.Background(), []byte("f"), int64(i), 64, 48); err != nil {
			t.Fatalf("tolerated timeout %d error = %v", i, err)
		}
	}
	failing = false
	if out, err := processor.Process(context.Background(), []byte("f"), 3, 64, 48); err != nil || string(out) != "ok" {
		t.Fatalf("recovery Process() = %q, %v; want \"ok\", nil", out, err)
	}
	if processor.FallbackActive() {
		t.Fatal("session latched despite a successful frame within the tolerated window")
	}

	// 초기화 뒤에는 다시 `threshold`번의 타임아웃을 허용한 다음에 잠가야 한다.
	failing = true
	for i := 0; i < threshold; i++ {
		if _, err := processor.Process(context.Background(), []byte("f"), int64(10+i), 64, 48); err != nil {
			t.Fatalf("post-reset timeout %d error = %v", i+1, err)
		}
		if processor.FallbackActive() {
			t.Fatalf("latched after %d post-reset timeouts, want tolerance up to %d", i+1, threshold)
		}
	}
}

func TestRealProcessorRawBlackoutFrame(t *testing.T) {
	ai := &fakeAIStream{process: func([]byte, int64) (*aiv1.ProcessedVideoChunk, error) {
		return nil, errors.New("ai unavailable")
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatRaw, config.FailurePolicyBlackoutLatch, 0)
	if err != nil {
		t.Fatal(err)
	}

	const width, height = 4, 4
	black, err := processor.Process(context.Background(), make([]byte, rawFrameSize(width, height)), 1, width, height)
	if err != nil {
		t.Fatal(err)
	}
	if len(black) != rawFrameSize(width, height) {
		t.Fatalf("raw blackout size = %d, want %d", len(black), rawFrameSize(width, height))
	}
	for i := 0; i < width*height; i++ {
		if black[i] != 0x10 {
			t.Fatalf("luma byte %d = %#x, want 0x10", i, black[i])
		}
	}
	for i := width * height; i < len(black); i++ {
		if black[i] != 0x80 {
			t.Fatalf("chroma byte %d = %#x, want 0x80", i, black[i])
		}
	}
}

func TestRealProcessorSelfHealsAfterAIRecovers(t *testing.T) {
	calls := 0
	failing := true
	ai := &fakeAIStream{process: func(_ []byte, ts int64) (*aiv1.ProcessedVideoChunk, error) {
		calls++
		if failing {
			return nil, errors.New("ai unavailable")
		}
		return &aiv1.ProcessedVideoChunk{Data: []byte("ok"), Timestamp: ts, StatusMessage: "success"}, nil
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyBlackoutLatch, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 프레임마다 시도해 주기를 기다리지 않고 회복을 관측한다.
	processor.recoveryProbeInterval = 0

	// 첫 실패에서 잠근다.
	if _, err := processor.Process(context.Background(), []byte("f"), 1, 64, 48); err != nil {
		t.Fatalf("Process() at latch = %v", err)
	}
	if !processor.FallbackActive() {
		t.Fatal("session did not latch after AI failure")
	}

	// 계속 실패하는 동안 세션은 잠긴 채로 시도를 이어 간다.
	if _, err := processor.Process(context.Background(), []byte("f"), 2, 64, 48); err != nil {
		t.Fatal(err)
	}
	if !processor.FallbackActive() {
		t.Fatal("session unlatched while AI still failing")
	}

	// AI가 회복한다. 다음 시도에서 잠금을 풀고 실제 출력을 돌려줘야 한다.
	failing = false
	out, err := processor.Process(context.Background(), []byte("f"), 3, 64, 48)
	if err != nil {
		t.Fatalf("recovery Process() error = %v", err)
	}
	if string(out) != "ok" {
		t.Fatalf("recovery output = %q, want %q", out, "ok")
	}
	if processor.FallbackActive() {
		t.Fatal("session still latched after the AI recovered")
	}
}

func TestRealProcessorLatchedProbeIntervalThrottlesAI(t *testing.T) {
	calls := 0
	ai := &fakeAIStream{process: func([]byte, int64) (*aiv1.ProcessedVideoChunk, error) {
		calls++
		return nil, errors.New("ai unavailable")
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyBlackoutLatch, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 기본 1초 주기를 그대로 둔다. 잠금 뒤 연달아 오는 프레임이 각각 AI를 부르면
	// 안 된다.
	if _, err := processor.Process(context.Background(), []byte("f"), 1, 64, 48); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("AI calls at latch = %d, want 1", calls)
	}
	for i := 2; i <= 10; i++ {
		if _, err := processor.Process(context.Background(), []byte("f"), int64(i), 64, 48); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("AI calls during sub-interval latched frames = %d, want still 1", calls)
	}
}

// 파이프라인을 내리면(해상도 전환·세션 종료) 진행 중이던 AI 요청은
// context.Canceled로 끝난다. 서버가 스스로 끊은 것이라 AI 실패로 latch하면
// 안 된다(#297).
func TestRealProcessorCanceledRequestDoesNotLatch(t *testing.T) {
	ai := &fakeAIStream{process: func([]byte, int64) (*aiv1.ProcessedVideoChunk, error) {
		return nil, fmt.Errorf("receive AI video frame: %w", context.Canceled)
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyBlackoutLatch, 0)
	if err != nil {
		t.Fatal(err)
	}

	output, err := processor.Process(context.Background(), []byte("frame"), 1, 64, 48)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Process() error = %v, want context.Canceled", err)
	}
	if output != nil {
		t.Fatal("canceled request should not serve a blackout frame")
	}
	if processor.FallbackActive() {
		t.Fatal("FallbackActive() = true after cancellation, want no latch")
	}
}

func TestRealProcessorLatchesRealFailureAfterCancellation(t *testing.T) {
	canceled := true
	ai := &fakeAIStream{process: func([]byte, int64) (*aiv1.ProcessedVideoChunk, error) {
		if canceled {
			return nil, fmt.Errorf("receive AI video frame: %w", context.Canceled)
		}
		return nil, errors.New("ai unavailable")
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyBlackoutLatch, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = processor.Process(context.Background(), []byte("frame"), 1, 64, 48)

	canceled = false
	if _, err := processor.Process(context.Background(), []byte("frame"), 2, 64, 48); err != nil {
		t.Fatalf("Process() after real AI failure error = %v, want blackout frame", err)
	}
	if !processor.FallbackActive() {
		t.Fatal("real AI failure after a cancellation should still latch")
	}
}
