package media

import (
	"context"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

type fakeAIStream struct {
	process func([]byte, int64) (*aiv1.ProcessedVideoChunk, error)
}

func (f *fakeAIStream) Process(data []byte, timestamp int64, _, _ uint16, _ string) (*aiv1.ProcessedVideoChunk, error) {
	return f.process(data, timestamp)
}
func (f *fakeAIStream) Close() {}

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
