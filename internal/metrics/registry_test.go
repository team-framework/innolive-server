package metrics

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestWritePrometheusIncludesLoadTestSeries(t *testing.T) {
	r := New()
	r.SetActiveSessions(2)
	r.IncFrameReceived("real")
	r.IncFrameProcessed("real")
	r.ObserveProcessing("real", 12*time.Millisecond)
	r.ObserveAI("real", 8*time.Millisecond)
	r.ObserveStage("grpc", 8*time.Millisecond)
	r.ObserveStage("decode", 2*time.Millisecond)
	r.IncRTPPacketsReceived("real")
	r.AddRTPSequenceGaps("real", 2)
	r.IncRTPPacketsRecovered("real")
	r.IncRTPIngressPacketDropped("real")
	r.IncRTPOutOfOrder("real")
	r.AddRTPPacketsDiscarded("real", 3)
	r.IncRTPFramesAssembled("real")
	r.IncEgressReconnectExhausted()
	r.IncEgressReconnectInputTimeout()

	var output bytes.Buffer
	r.WritePrometheus(&output)
	text := output.String()
	for _, expected := range []string{
		"# TYPE process_cpu_seconds_total counter",
		"# TYPE process_resident_memory_bytes gauge",
		"# TYPE innolive_process_tree_cpu_seconds_total counter",
		"# TYPE innolive_process_tree_resident_memory_bytes gauge",
		"innolive_active_sessions 2",
		`innolive_frame_received_total{mode="real"} 1`,
		"innolive_frame_processing_duration_seconds_bucket",
		"innolive_ai_duration_seconds_bucket",
		`innolive_ai_stage_duration_seconds_bucket{stage="grpc"`,
		"innolive_decode_duration_seconds_bucket",
		`innolive_rtp_packets_received_total{mode="real"} 1`,
		`innolive_rtp_sequence_gaps_total{mode="real"} 2`,
		`innolive_rtp_packets_recovered_total{mode="real"} 1`,
		`innolive_rtp_ingress_packets_dropped_total{mode="real"} 1`,
		`innolive_rtp_out_of_order_total{mode="real"} 1`,
		`innolive_rtp_packets_discarded_total{mode="real"} 3`,
		`innolive_rtp_frames_assembled_total{mode="real"} 1`,
		"innolive_egress_reconnect_exhausted_total 1",
		"innolive_egress_reconnect_input_timeout_total 1",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("metrics output does not contain %q\n%s", expected, text)
		}
	}
}

func TestProcessTreeKeepsCompletedChildCPU(t *testing.T) {
	r := New()
	r.RegisterChildProcess(999999)
	r.CompleteChildProcess(999999, 2.5)

	cpuSeconds, rssBytes := r.processTreeStats(1.25, 1024)
	if cpuSeconds != 3.75 {
		t.Fatalf("process tree CPU = %v, want 3.75", cpuSeconds)
	}
	if rssBytes != 1024 {
		t.Fatalf("process tree RSS = %d, want 1024", rssBytes)
	}
}
