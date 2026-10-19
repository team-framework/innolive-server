package media

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

// fakePipelineStream은 미리 보내기(AIPipeline)를 지원하는 AI 스트림이다. 응답은 wait가
// 정하고, 보냈지만 응답을 돌려주지 않은 요청 수의 최댓값을 센다(#495).
type fakePipelineStream struct {
	t    *testing.T
	wait func(timestamp int64) (*aiv1.ProcessedVideoChunk, error)

	mu          sync.Mutex
	submitted   []int64
	outstanding int
	maxInflight int
}

func (f *fakePipelineStream) Process([]byte, int64, uint16, uint16, string, *aiv1.MosaicConfig) (*aiv1.ProcessedVideoChunk, error) {
	f.t.Error("Process must not be used once the stream sends ahead")
	return nil, fmt.Errorf("unexpected Process")
}

func (f *fakePipelineStream) Close() {}

func (f *fakePipelineStream) SubmitFrame(_ []byte, timestamp int64, _, _ uint16, _ string, _ *aiv1.MosaicConfig) (func() (*aiv1.ProcessedVideoChunk, error), error) {
	f.mu.Lock()
	f.submitted = append(f.submitted, timestamp)
	f.outstanding++
	if f.outstanding > f.maxInflight {
		f.maxInflight = f.outstanding
	}
	f.mu.Unlock()
	return func() (*aiv1.ProcessedVideoChunk, error) {
		response, err := f.wait(timestamp)
		f.mu.Lock()
		f.outstanding--
		f.mu.Unlock()
		return response, err
	}, nil
}

func (f *fakePipelineStream) submittedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.submitted)
}

func success(timestamp int64) *aiv1.ProcessedVideoChunk {
	return &aiv1.ProcessedVideoChunk{Data: []byte(fmt.Sprintf("out-%d", timestamp)), Timestamp: timestamp, StatusMessage: "success"}
}

func TestBeginSubmitsAheadAndFinishesEachFrameWithItsOwnResponse(t *testing.T) {
	release := map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{})}
	ai := &fakePipelineStream{t: t, wait: func(timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
		<-release[timestamp]
		return success(timestamp), nil
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyFreeze, 0)
	if err != nil {
		t.Fatal(err)
	}
	processor.SetAIMaxInflight(2)

	first := processor.BeginIfAIInputEnabled(context.Background(), []byte("a"), 1, 0, 0)
	second := processor.BeginIfAIInputEnabled(context.Background(), []byte("b"), 2, 0, 0)
	if got := ai.submittedCount(); got != 2 {
		t.Fatalf("submitted before any response = %d, want 2", got)
	}
	close(release[2])
	close(release[1])
	for index, finish := range []func() ([]byte, bool, error){first, second} {
		output, processed, err := finish()
		if err != nil || !processed || string(output) != fmt.Sprintf("out-%d", index+1) {
			t.Fatalf("frame %d finish = %q, %v, %v", index+1, output, processed, err)
		}
	}
}

func TestSetAIMaxInflightStaysOneWithoutAPipelineStream(t *testing.T) {
	var calls int
	ai := &fakeAIStream{process: func(_ []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
		calls++
		return success(timestamp), nil
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyFreeze, 0)
	if err != nil {
		t.Fatal(err)
	}
	processor.SetAIMaxInflight(2)
	if got := processor.AIMaxInflight(); got != 1 {
		t.Fatalf("AIMaxInflight() = %d, want 1 for a stream that cannot send ahead", got)
	}
	finish := processor.BeginIfAIInputEnabled(context.Background(), []byte("a"), 7, 0, 0)
	if calls != 1 {
		t.Fatalf("AI calls after Begin = %d, want the frame processed synchronously", calls)
	}
	if output, _, err := finish(); err != nil || string(output) != "out-7" {
		t.Fatalf("finish = %q, %v", output, err)
	}
}

// 미리 보낸 프레임의 타임아웃도 순차 처리와 같은 기준으로 세어 세션을 잠근다.
func TestPipelinedTimeoutsLatchLikeSequentialFrames(t *testing.T) {
	const width, height = 8, 8
	ai := &fakePipelineStream{t: t, wait: func(int64) (*aiv1.ProcessedVideoChunk, error) {
		return nil, fmt.Errorf("receive AI video frame: %w", context.DeadlineExceeded)
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatRaw, config.FailurePolicyBlackoutLatch, 1)
	if err != nil {
		t.Fatal(err)
	}
	processor.SetAIMaxInflight(2)
	frame := checkerboardRawFrame(width, height, 2, 0)

	first := processor.BeginIfAIInputEnabled(context.Background(), frame, 1, width, height)
	second := processor.BeginIfAIInputEnabled(context.Background(), frame, 2, width, height)
	for index, finish := range []func() ([]byte, bool, error){first, second} {
		if output, _, err := finish(); err != nil || len(output) != len(frame) {
			t.Fatalf("frame %d must fall back to a blurred frame, got %d bytes, %v", index+1, len(output), err)
		}
	}
	if !processor.FallbackActive() {
		t.Fatal("two timeouts past a threshold of one must latch the session")
	}
	third := processor.BeginIfAIInputEnabled(context.Background(), frame, 3, width, height)
	if output, _, err := third(); err != nil || len(output) != len(frame) {
		t.Fatalf("latched frame = %d bytes, %v", len(output), err)
	}
	if got := ai.submittedCount(); got != 2 {
		t.Fatalf("submitted = %d, want no AI request between recovery probes", got)
	}
}

func TestProcessImagesPipelinedKeepsFrameOrderAndBoundsInflight(t *testing.T) {
	const frames, inflight = 12, 2
	registry := metrics.New()
	ai := &fakePipelineStream{t: t, wait: func(timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
		// 앞 프레임일수록 늦게 끝나도 내보내는 순서는 받은 순서여야 한다.
		time.Sleep(time.Duration(frames-timestamp%frames) * 200 * time.Microsecond)
		return success(timestamp), nil
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, registry, nil, config.WireFormatJPEG, config.FailurePolicyFreeze, 0)
	if err != nil {
		t.Fatal(err)
	}
	processor.SetAIMaxInflight(inflight)

	decoded := make(chan frame, frames)
	for index := 0; index < frames; index++ {
		decoded <- frame{data: []byte(fmt.Sprintf("in-%d", index)), width: 640, height: 480, timestamp: uint32(index)}
	}
	close(decoded)
	processed := make(chan frame, frames)
	processImages(context.Background(), nil, processor, decoded, processed, NewEgressFanout(), registry, config.PrivacyModeReal, nil)

	var order []uint32
	for item := range processed {
		order = append(order, item.timestamp)
	}
	if len(order) != frames {
		t.Fatalf("processed %d frames, want %d", len(order), frames)
	}
	for index, timestamp := range order {
		if timestamp != uint32(index) {
			t.Fatalf("processed order = %v, want input order", order)
		}
	}
	if ai.maxInflight > inflight {
		t.Fatalf("frames in flight reached %d, want at most %d", ai.maxInflight, inflight)
	}
	if ai.maxInflight < inflight {
		t.Fatalf("frames in flight peaked at %d; the loop never sent ahead", ai.maxInflight)
	}
}
