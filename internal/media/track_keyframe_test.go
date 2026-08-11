package media

import (
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

// newLossyAssembler builds an assembler whose reorder window is small enough
// that a sequence jump forces the sample builder to give up on the gap and
// report PrevDroppedPackets.
func newLossyAssembler(t *testing.T, minInterval time.Duration, requestKeyframe func()) *rtpFrameAssembler {
	t.Helper()
	assembler, err := newRTPFrameAssemblerWithLimits(
		metrics.New(),
		config.PrivacyModeBypass,
		VideoCodecVP8,
		4,
		10*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	assembler.requestKeyframe = requestKeyframe
	assembler.keyframeMinInterval = minInterval
	return assembler
}

// pushLoss feeds an incomplete frame followed by a far-ahead complete one so
// the sample builder has to discard the partial frame's packets.
func pushLoss(assembler *rtpFrameAssembler, base uint16, timestamp uint32) {
	assembler.push(vp8Packet(base, timestamp, false, true, "partial"))
	for offset := uint16(1); offset <= 8; offset++ {
		assembler.push(vp8Packet(base+100+offset, timestamp+uint32(offset)*3000, true, true, "whole"))
	}
}

// TestAssemblerRequestsKeyframeAfterDroppedPackets covers the recovery path for
// #93: discarded RTP packets mean the decoder just lost its reference, and the
// damage is re-encoded into every outgoing frame — including keyframes — until
// the publisher sends a fresh one. Counting the loss without asking for a
// keyframe leaves the picture broken for as long as the publisher's own
// keyframe interval (measured ~20s with Chrome).
func TestAssemblerRequestsKeyframeAfterDroppedPackets(t *testing.T) {
	var requests int
	assembler := newLossyAssembler(t, 0, func() { requests++ })

	pushLoss(assembler, 100, 3000)

	if requests == 0 {
		t.Fatalf("assembler discarded packets without requesting a keyframe")
	}
}

// TestAssemblerThrottlesKeyframeRequests guards against flooding the publisher:
// a single lost gap surfaces across several samples, and the publisher needs
// time to encode and deliver the keyframe before another request is useful.
func TestAssemblerThrottlesKeyframeRequests(t *testing.T) {
	var requests int
	assembler := newLossyAssembler(t, time.Hour, func() { requests++ })

	pushLoss(assembler, 100, 3000)
	pushLoss(assembler, 500, 90000)

	if requests != 1 {
		t.Fatalf("keyframe requests = %d, want 1 within the throttle window", requests)
	}
}

// TestAssemblerWithoutKeyframeRequesterDoesNotPanic keeps the assembler usable
// for callers that have no feedback channel to the publisher.
func TestAssemblerWithoutKeyframeRequesterDoesNotPanic(t *testing.T) {
	assembler := newLossyAssembler(t, 0, nil)
	pushLoss(assembler, 100, 3000)
}

// TestAssemblerDoesNotRequestKeyframeWithoutLoss makes sure a clean stream
// never asks the publisher for anything.
func TestAssemblerDoesNotRequestKeyframeWithoutLoss(t *testing.T) {
	var requests int
	assembler := newLossyAssembler(t, 0, func() { requests++ })

	for offset := uint16(0); offset < 8; offset++ {
		assembler.push(vp8Packet(100+offset, 3000+uint32(offset)*3000, true, true, "whole"))
	}

	if requests != 0 {
		t.Fatalf("keyframe requests = %d on a lossless stream, want 0", requests)
	}
}
