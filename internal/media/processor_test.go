package media

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

type fakeAIStream struct {
	process func([]byte, int64) (*aiv1.ProcessedVideoChunk, error)
	close   func()
}

func (f *fakeAIStream) Process(data []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
	return f.process(data, timestamp)
}
func (f *fakeAIStream) Close() {
	if f.close != nil {
		f.close()
	}
}

func TestRealProcessorCallsAIWithImage(t *testing.T) {
	input := []byte("jpeg-input")
	ai := &fakeAIStream{process: func(data []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
		if string(data) != "jpeg-input" {
			t.Fatalf("AI input = %q", data)
		}
		return &aiv1.ProcessedVideoChunk{Data: []byte("jpeg-output"), Timestamp: timestamp, StatusMessage: "success"}, nil
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyFreeze, 0)
	if err != nil {
		t.Fatal(err)
	}
	output, err := processor.Process(context.Background(), input, 123, 0, 0)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if string(output) != "jpeg-output" {
		t.Fatalf("output = %q", output)
	}
}

// TestProcessorPauseStopsAIInputUntilResume은 pause 응답이 반환된 뒤에는
// 카메라 프레임이 AI worker에 도달하지 않고, resume 뒤 다음 프레임에서 다시
// 처리되는 계약을 검증한다.
func TestProcessorPauseStopsAIInputUntilResume(t *testing.T) {
	var calls atomic.Int64
	var closes atomic.Int64
	ai := &fakeAIStream{
		process: func(_ []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
			calls.Add(1)
			return &aiv1.ProcessedVideoChunk{Data: []byte("jpeg-output"), Timestamp: timestamp, StatusMessage: "success"}, nil
		},
		close: func() { closes.Add(1) },
	}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyFreeze, 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, processed, err := processor.ProcessIfAIInputEnabled(context.Background(), []byte("before-pause"), 1, 0, 0); err != nil || !processed {
		t.Fatalf("before pause = (processed=%v, err=%v), want (true, nil)", processed, err)
	}
	processor.SuspendAIInput()
	if !processor.AIInputPaused() {
		t.Fatal("AI input must be paused")
	}
	if _, processed, err := processor.ProcessIfAIInputEnabled(context.Background(), []byte("while-paused"), 2, 0, 0); err != nil || processed {
		t.Fatalf("while paused = (processed=%v, err=%v), want (false, nil)", processed, err)
	}
	if _, err := processor.Process(context.Background(), []byte("direct-while-paused"), 3, 0, 0); !errors.Is(err, errAIInputPaused) {
		t.Fatalf("direct Process while paused error = %v, want errAIInputPaused", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("AI Process calls while paused = %d, want 1 total", got)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("AI stream Close calls after pause = %d, want 1", got)
	}

	processor.ResumeAIInput()
	if processor.AIInputPaused() {
		t.Fatal("AI input must be resumed")
	}
	if _, processed, err := processor.ProcessIfAIInputEnabled(context.Background(), []byte("after-resume"), 3, 0, 0); err != nil || !processed {
		t.Fatalf("after resume = (processed=%v, err=%v), want (true, nil)", processed, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("AI Process calls after resume = %d, want 2", got)
	}
}

func TestProcessorDisablingAnonymizationPassesFramesWithoutClosingAIStream(t *testing.T) {
	var calls atomic.Int64
	var closes atomic.Int64
	processor, err := NewProcessor(config.PrivacyModeReal, 0, &fakeAIStream{
		process: func(_ []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
			calls.Add(1)
			return &aiv1.ProcessedVideoChunk{Data: []byte("anonymized"), Timestamp: timestamp, StatusMessage: "success"}, nil
		},
		close: func() { closes.Add(1) },
	}, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyFreeze, 0)
	if err != nil {
		t.Fatal(err)
	}

	processor.SetAnonymizationEnabled(false)
	if processor.AnonymizationEnabled() {
		t.Fatal("anonymization must be disabled")
	}
	output, processed, err := processor.ProcessIfAIInputEnabled(context.Background(), []byte("raw-frame"), 1, 0, 0)
	if err != nil || !processed || string(output) != "raw-frame" {
		t.Fatalf("disabled output = (%q, processed=%v, err=%v), want raw frame", output, processed, err)
	}
	if calls.Load() != 0 || closes.Load() != 0 {
		t.Fatalf("disabled anonymization calls=%d closes=%d, want both 0", calls.Load(), closes.Load())
	}

	processor.SetAnonymizationEnabled(true)
	output, processed, err = processor.ProcessIfAIInputEnabled(context.Background(), []byte("next-frame"), 2, 0, 0)
	if err != nil || !processed || string(output) != "anonymized" {
		t.Fatalf("enabled output = (%q, processed=%v, err=%v), want anonymized frame", output, processed, err)
	}
	if calls.Load() != 1 || closes.Load() != 0 {
		t.Fatalf("enabled anonymization calls=%d closes=%d, want 1 and 0", calls.Load(), closes.Load())
	}
}

// TestProcessorSuspendWaitsForInFlightAIFrame은 pause 성공 응답 뒤에 이미
// 통과한 프레임까지 AI worker로 전송되는 경합이 없도록, 진행 중인 호출이
// 끝날 때까지 SuspendAIInput이 기다리는지 검증한다.
func TestProcessorSuspendWaitsForInFlightAIFrame(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ai := &fakeAIStream{process: func(_ []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
		close(started)
		<-release
		return &aiv1.ProcessedVideoChunk{Data: []byte("jpeg-output"), Timestamp: timestamp, StatusMessage: "success"}, nil
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyFreeze, 0)
	if err != nil {
		t.Fatal(err)
	}

	processed := make(chan struct{})
	go func() {
		defer close(processed)
		_, _, _ = processor.ProcessIfAIInputEnabled(context.Background(), []byte("in-flight"), 1, 0, 0)
	}()
	<-started

	suspended := make(chan struct{})
	go func() {
		processor.SuspendAIInput()
		close(suspended)
	}()
	select {
	case <-suspended:
		t.Fatal("SuspendAIInput returned before the in-flight AI frame completed")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	<-processed
	<-suspended
	if _, accepted, err := processor.ProcessIfAIInputEnabled(context.Background(), []byte("after-pause"), 2, 0, 0); err != nil || accepted {
		t.Fatalf("after pause = (accepted=%v, err=%v), want (false, nil)", accepted, err)
	}
}

// TestProcessImageRejectsNonEmptyErrorCode guards the fail-closed path added
// for the AI server's error_code field: the RPC can succeed at the transport
// level (status_message=="success"-shaped response) while still reporting a
// per-frame failure via error_code, and that must be treated as a failure,
// not a successfully processed frame.
func TestProcessImageRejectsNonEmptyErrorCode(t *testing.T) {
	ai := &fakeAIStream{process: func(data []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
		return &aiv1.ProcessedVideoChunk{
			Data:         []byte("out"),
			Timestamp:    timestamp,
			ErrorCode:    "decode_failed",
			ErrorMessage: "could not decode frame",
		}, nil
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyFreeze, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := processor.ProcessImage([]byte("in"), 1); err == nil {
		t.Fatal("ProcessImage() error = nil, want a failure for non-empty error_code")
	}
}

func TestBypassProcessorPreservesDecodedFrame(t *testing.T) {
	input := []byte("decoded-jpeg")
	processor, err := NewProcessor(config.PrivacyModeBypass, 0, nil, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyBlackoutLatch, 0)
	if err != nil {
		t.Fatal(err)
	}
	output, err := processor.Process(context.Background(), input, 123, 0, 0)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if string(output) != string(input) {
		t.Fatalf("output = %q, want %q", output, input)
	}
}

func TestFixedDelayProcessorWaits(t *testing.T) {
	processor, err := NewProcessor(config.PrivacyModeFixedDelay, 10*time.Millisecond, nil, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyBlackoutLatch, 0)
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now()
	input := []byte{1}
	output, err := processor.Process(context.Background(), input, 1, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != string(input) {
		t.Fatalf("output = %v, want %v", output, input)
	}
	if elapsed := time.Since(startedAt); elapsed < 9*time.Millisecond {
		t.Fatalf("fixed delay elapsed = %s", elapsed)
	}
}
