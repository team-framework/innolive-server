package media

import (
	"bytes"
	"context"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"

	"github.com/pion/rtp"
)

func TestRTPSequenceTrackerRecordsGapRecoveryAndOutOfOrder(t *testing.T) {
	var tracker rtpSequenceTracker

	if observation := tracker.observe(100); observation != (rtpSequenceObservation{}) {
		t.Fatalf("initial observation = %+v, want empty", observation)
	}
	if observation := tracker.observe(102); observation.gap != 1 {
		t.Fatalf("gap observation = %+v, want gap=1", observation)
	}
	if observation := tracker.observe(101); !observation.recovered {
		t.Fatalf("late missing packet observation = %+v, want recovered", observation)
	}
	if observation := tracker.observe(101); !observation.outOfOrder {
		t.Fatalf("duplicate packet observation = %+v, want out-of-order", observation)
	}
}

func TestEgressSlotClearIfKeepsReplacement(t *testing.T) {
	slot := NewEgressSlot()
	previous := &RTMPEgress{}
	replacement := &RTMPEgress{}
	slot.Set(previous)
	slot.Set(replacement)

	if slot.ClearIf(previous) {
		t.Fatal("ClearIf must not clear a replacement egress")
	}
	if got := slot.Load(); got != replacement {
		t.Fatalf("slot.Load() = %p, want replacement %p", got, replacement)
	}
	if !slot.ClearIf(replacement) {
		t.Fatal("ClearIf must clear the current egress")
	}
	if got := slot.Load(); got != nil {
		t.Fatalf("slot.Load() = %p after clear, want nil", got)
	}
}

// TestProcessImagesSkipsPausedAIInput은 실제 트랙 처리 루프가 pause 중에는
// Processor.Process를 호출하지 않고, 프레임을 AI 입력 차단 metric으로만
// 기록하는지 검증한다.
func TestProcessImagesSkipsPausedAIInput(t *testing.T) {
	registry := metrics.New()
	var calls int
	processor, err := NewProcessor(
		config.PrivacyModeReal,
		0,
		&fakeAIStream{process: func(_ []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
			calls++
			return &aiv1.ProcessedVideoChunk{Data: []byte("output"), Timestamp: timestamp, StatusMessage: "success"}, nil
		}},
		registry,
		nil,
		config.WireFormatJPEG,
		config.FailurePolicyFreeze,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	processor.SuspendAIInput()

	decoded := make(chan frame, 1)
	decoded <- frame{data: []byte("camera-frame"), width: 640, height: 480}
	close(decoded)
	processed := make(chan frame, 1)
	processImages(context.Background(), nil, processor, decoded, processed, NewEgressSlot(), registry, config.PrivacyModeReal, nil)

	if calls != 0 {
		t.Fatalf("AI Process calls = %d, want 0 while paused", calls)
	}
	if _, ok := <-processed; ok {
		t.Fatal("paused frame must not reach the processed output")
	}
	var metricsOutput bytes.Buffer
	registry.WritePrometheus(&metricsOutput)
	if !bytes.Contains(metricsOutput.Bytes(), []byte(`innolive_ai_input_paused_frames_total{mode="real"} 1`)) {
		t.Fatalf("paused AI input metric missing\n%s", metricsOutput.String())
	}
}

func TestRTPFrameAssemblerReordersLateVP8Packet(t *testing.T) {
	registry := metrics.New()
	assembler, err := newRTPFrameAssemblerWithLimits(
		registry,
		config.PrivacyModeBypass,
		VideoCodecVP8,
		16,
		100*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}

	var completed []frame
	var tracker rtpSequenceTracker
	for _, packet := range []*rtp.Packet{
		vp8Packet(100, 3000, false, true, "A"),
		vp8Packet(102, 3000, true, false, "C"),
		vp8Packet(101, 3000, false, false, "B"),
		vp8Packet(103, 6000, true, true, "D"),
	} {
		observeRTPPacket(registry, "bypass", &tracker, packet)
		completed = append(completed, assembler.push(packet)...)
	}

	if len(completed) != 1 {
		t.Fatalf("assembled frames = %d, want 1", len(completed))
	}
	if got := string(completed[0].data); got != "ABC" {
		t.Fatalf("assembled payload = %q, want %q", got, "ABC")
	}
	if completed[0].timestamp != 3000 {
		t.Fatalf("assembled timestamp = %d, want 3000", completed[0].timestamp)
	}

	var output bytes.Buffer
	registry.WritePrometheus(&output)
	for _, expected := range []string{
		`innolive_rtp_packets_received_total{mode="bypass"} 4`,
		`innolive_rtp_sequence_gaps_total{mode="bypass"} 1`,
		`innolive_rtp_packets_recovered_total{mode="bypass"} 1`,
		`innolive_rtp_frames_assembled_total{mode="bypass"} 1`,
	} {
		if !bytes.Contains(output.Bytes(), []byte(expected)) {
			t.Errorf("metrics output does not contain %q\n%s", expected, output.String())
		}
	}
}

func TestEnqueueLatestRTPPacketDoesNotBlockWhenFull(t *testing.T) {
	registry := metrics.New()
	output := make(chan *rtp.Packet, 2)
	ctx := context.Background()

	for sequence := uint16(1); sequence <= 3; sequence++ {
		if !enqueueLatestRTPPacket(
			ctx,
			output,
			&rtp.Packet{Header: rtp.Header{SequenceNumber: sequence}},
			registry,
			"bypass",
		) {
			t.Fatal("enqueue returned false")
		}
	}

	first := <-output
	second := <-output
	if first.SequenceNumber != 2 || second.SequenceNumber != 3 {
		t.Fatalf("buffered sequences = %d,%d, want 2,3", first.SequenceNumber, second.SequenceNumber)
	}

	var metricsOutput bytes.Buffer
	registry.WritePrometheus(&metricsOutput)
	expected := `innolive_rtp_ingress_packets_dropped_total{mode="bypass"} 1`
	if !bytes.Contains(metricsOutput.Bytes(), []byte(expected)) {
		t.Fatalf("metrics output does not contain %q\n%s", expected, metricsOutput.String())
	}
}

func vp8Packet(sequence uint16, timestamp uint32, marker, head bool, payload string) *rtp.Packet {
	descriptor := byte(0)
	if head {
		descriptor = 0x10
	}
	return &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: sequence,
			Timestamp:      timestamp,
			Marker:         marker,
		},
		Payload: append([]byte{descriptor}, []byte(payload)...),
	}
}
