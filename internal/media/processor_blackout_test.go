package media

import (
	"bytes"
	"context"
	"errors"
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

	// The first `threshold` consecutive timeouts are tolerated: a blackout frame
	// is served, but the session is NOT latched and the AI keeps being retried.
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

	// Exceeding the threshold latches permanently.
	if _, err := processor.Process(context.Background(), []byte("frame"), 99, 64, 48); err != nil {
		t.Fatalf("Process() at latch error = %v", err)
	}
	if !processor.FallbackActive() {
		t.Fatal("session did not latch after exceeding the timeout threshold")
	}
	if calls != threshold+1 {
		t.Fatalf("AI calls at latch = %d, want %d", calls, threshold+1)
	}

	// Once latched, the AI is no longer called.
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
	// A generous threshold must not protect a non-timeout (definitive) failure:
	// it latches on the very first one, as before.
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

	// Two timeouts (within threshold), then a success must reset the streak.
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

	// After the reset it must again tolerate `threshold` timeouts before latching.
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
