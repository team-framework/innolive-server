package media

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

func newTestEgress(wire config.WireFormat, url string) *RTMPEgress {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewRTMPEgress("ffmpeg", logger, metrics.New(), TranscoderOptions{WireFormat: wire}, url)
}

func TestMeasureFPS(t *testing.T) {
	// framesAt builds n frames whose RTP timestamps advance by step (90kHz clock).
	framesAt := func(n int, start uint32, step uint32) []frame {
		out := make([]frame, n)
		ts := start
		for i := range out {
			out[i] = frame{timestamp: ts}
			ts += step
		}
		return out
	}

	tests := []struct {
		name  string
		input []frame
		want  int
	}{
		{"30fps", framesAt(31, 0, videoClockRate/30), 30},
		{"60fps", framesAt(31, 0, videoClockRate/60), 60},
		{"24fps", framesAt(25, 1000, videoClockRate/24), 24},
		{"single frame falls back", framesAt(1, 0, 3000), egressDefaultFPS},
		{"zero span falls back", framesAt(10, 5000, 0), egressDefaultFPS},
		{"too high falls back", framesAt(10, 0, videoClockRate/180), egressDefaultFPS},
		{"timestamp wraparound", framesAt(31, 0xFFFFFC18, videoClockRate/30), 30},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := measureFPS(tc.input); got != tc.want {
				t.Fatalf("measureFPS = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMaskStreamKey(t *testing.T) {
	tests := map[string]string{
		"rtmp://a.rtmp.youtube.com/live2/ss0y-abcd-1234": "rtmp://a.rtmp.youtube.com/live2/****",
		"rtmp://host/live/key":                           "rtmp://host/live/****",
		"rtmp://host/live/":                              "rtmp://host/live/", // trailing slash: nothing to mask
		"out.flv":                                        "out.flv",           // no slash
	}
	for in, want := range tests {
		if got := maskStreamKey(in); got != want {
			t.Errorf("maskStreamKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsProgressKey(t *testing.T) {
	progress := []string{"frame", "fps", "bitrate", "out_time_ms", "dup_frames", "drop_frames", "speed", "progress", "stream_0_0_q"}
	for _, k := range progress {
		if !isProgressKey(k) {
			t.Errorf("isProgressKey(%q) = false, want true", k)
		}
	}
	notProgress := []string{"Connection refused", "[rtmp @ 0x1]", "Error", "broken pipe"}
	for _, k := range notProgress {
		if isProgressKey(k) {
			t.Errorf("isProgressKey(%q) = true, want false", k)
		}
	}
}

func TestValidFrame(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0x00, 0x11, 0xFF, 0xD9}
	notJPEG := []byte{0x00, 0x01, 0x02, 0x03}

	je := newTestEgress(config.WireFormatJPEG, "out.flv")
	if !je.validFrame(frame{data: jpeg}) {
		t.Error("JPEG frame should be valid in jpeg mode")
	}
	if je.validFrame(frame{data: notJPEG}) {
		t.Error("non-JPEG frame should be invalid in jpeg mode")
	}

	re := newTestEgress(config.WireFormatRaw, "out.flv")
	const w, h = 4, 4
	raw := make([]byte, rawFrameSize(w, h))
	if !re.validFrame(frame{data: raw, width: w, height: h}) {
		t.Error("correctly sized raw frame should be valid")
	}
	if re.validFrame(frame{data: raw[:len(raw)-1], width: w, height: h}) {
		t.Error("undersized raw frame should be invalid")
	}
}

func TestEnqueueDropsOldest(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "out.flv")
	// Fill past capacity; queue holds egressQueueSize (5). Oldest must be dropped.
	total := egressQueueSize + 2 // 7 frames -> expect the last 5 retained
	for i := 1; i <= total; i++ {
		e.Enqueue(frame{timestamp: uint32(i)})
	}
	if len(e.input) != egressQueueSize {
		t.Fatalf("queue length = %d, want %d", len(e.input), egressQueueSize)
	}
	var got []uint32
	for len(e.input) > 0 {
		got = append(got, (<-e.input).timestamp)
	}
	want := []uint32{3, 4, 5, 6, 7}
	if len(got) != len(want) {
		t.Fatalf("drained %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("drained %v, want %v (drop-oldest violated)", got, want)
		}
	}
	// Two frames were dropped; the metric must reflect it.
	if n := egressDroppedCount(e.metrics); n != 2 {
		t.Fatalf("egress dropped metric = %d, want 2", n)
	}
}

func TestHandleStderrLineParsesProgress(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "rtmp://host/live2/secretkey")
	e.handleStderrLine("dup_frames=7")
	e.handleStderrLine("drop_frames=4")
	e.handleStderrLine("fps=30.0") // progress but not a counter we store
	out := prometheusDump(e.metrics)
	if !strings.Contains(out, "innolive_egress_dup_frames 7") {
		t.Errorf("dup_frames gauge not set:\n%s", out)
	}
	if !strings.Contains(out, "innolive_egress_drop_frames 4") {
		t.Errorf("drop_frames gauge not set:\n%s", out)
	}
}

func TestHandleStderrLineMasksKeyInErrors(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "rtmp://a.rtmp.youtube.com/live2/secretkey123")
	var buf bytes.Buffer
	e.logger = slog.New(slog.NewTextHandler(&buf, nil))
	e.handleStderrLine("Error opening output rtmp://a.rtmp.youtube.com/live2/secretkey123: Connection refused")
	logged := buf.String()
	if strings.Contains(logged, "secretkey123") {
		t.Errorf("stream key leaked into logs: %s", logged)
	}
	if !strings.Contains(logged, "live2/****") {
		t.Errorf("masked URL not present in logs: %s", logged)
	}
}

// egressDroppedCount extracts the innolive_egress_frames_dropped_total counter.
func egressDroppedCount(r *metrics.Registry) int {
	out := prometheusDump(r)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "innolive_egress_frames_dropped_total ") {
			var n int
			// value is the trailing field
			fields := strings.Fields(line)
			if len(fields) == 2 {
				for _, c := range fields[1] {
					n = n*10 + int(c-'0')
				}
			}
			return n
		}
	}
	return -1
}

func prometheusDump(r *metrics.Registry) string {
	var buf bytes.Buffer
	r.WritePrometheus(&buf)
	return buf.String()
}
