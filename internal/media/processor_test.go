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

	lastWidth, lastHeight uint16
	lastPixFmt            string
}

func (f *fakeAIStream) Process(data []byte, timestamp int64, width, height uint16, pixFmt string) (*aiv1.ProcessedVideoChunk, error) {
	f.lastWidth, f.lastHeight, f.lastPixFmt = width, height, pixFmt
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

// TestProcessImageJPEGModeSendsZeroDimensions guards against a real bug: some
// AI servers reuse VideoChunk's width/height field numbers for unrelated data
// (e.g. batch_size). jpeg is self-describing, so width/height must stay zero
// in jpeg mode regardless of the frame's actual decoded size.
func TestProcessImageJPEGModeSendsZeroDimensions(t *testing.T) {
	ai := &fakeAIStream{process: func(data []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
		return &aiv1.ProcessedVideoChunk{Data: []byte("out"), Timestamp: timestamp, StatusMessage: "success"}, nil
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatJPEG, config.FailurePolicyFreeze, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := processor.ProcessImage([]byte("in"), 1, 1280, 720); err != nil {
		t.Fatalf("ProcessImage() error = %v", err)
	}
	if ai.lastWidth != 0 || ai.lastHeight != 0 {
		t.Fatalf("jpeg mode sent width=%d height=%d, want 0,0", ai.lastWidth, ai.lastHeight)
	}
	if ai.lastPixFmt != "" {
		t.Fatalf("jpeg mode sent pix_fmt=%q, want empty", ai.lastPixFmt)
	}
}

// TestProcessImageRawModeSendsRealDimensions confirms the fix above didn't
// zero out width/height for the raw wire format, which needs them to decode.
func TestProcessImageRawModeSendsRealDimensions(t *testing.T) {
	ai := &fakeAIStream{process: func(data []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
		return &aiv1.ProcessedVideoChunk{Data: []byte("out"), Timestamp: timestamp, StatusMessage: "success"}, nil
	}}
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, metrics.New(), nil, config.WireFormatRaw, config.FailurePolicyFreeze, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := processor.ProcessImage([]byte("in"), 1, 1280, 720); err != nil {
		t.Fatalf("ProcessImage() error = %v", err)
	}
	if ai.lastWidth != 1280 || ai.lastHeight != 720 {
		t.Fatalf("raw mode sent width=%d height=%d, want 1280,720", ai.lastWidth, ai.lastHeight)
	}
	if ai.lastPixFmt != "yuv420p" {
		t.Fatalf("raw mode sent pix_fmt=%q, want yuv420p", ai.lastPixFmt)
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
