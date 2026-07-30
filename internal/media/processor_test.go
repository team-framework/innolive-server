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

func (f *fakeAIStream) Process(data []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
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
