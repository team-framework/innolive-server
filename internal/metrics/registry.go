package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var defaultBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.02, 0.033, 0.05, 0.075, 0.1, 0.2, 0.5, 1}

type histogram struct {
	count   uint64
	sum     float64
	buckets []uint64
}

type Registry struct {
	activeSessions    atomic.Int64
	connections       atomic.Uint64
	connectionFailure atomic.Uint64
	reconnects        atomic.Uint64
	peerRecoveryStart atomic.Uint64
	peerRecoveryOK    atomic.Uint64
	peerRecoveryEnd   atomic.Uint64
	sessionsRejected  atomic.Uint64
	queueSize         atomic.Int64
	egressReconnects  atomic.Uint64
	egressExhausted   atomic.Uint64
	egressInputTimed  atomic.Uint64
	egressDropped     atomic.Uint64
	egressDupFrames   atomic.Int64
	egressDropFrames  atomic.Int64
	audioSamplesOut   atomic.Uint64
	audioSamplesDrop  atomic.Uint64
	processTree       processTreeTracker

	mu                  sync.RWMutex
	aiFallbackLatched   map[string]uint64
	aiFallbackFrames    map[string]uint64
	aiInputPausedFrames map[string]uint64
	aiTargetSessions    map[string]uint64
	framesReceived      map[string]uint64
	framesProcessed     map[string]uint64
	framesDropped       map[string]uint64
	frameFailures       map[string]uint64
	rtpPacketsReceived  map[string]uint64
	rtpSequenceGaps     map[string]uint64
	rtpPacketsRecovered map[string]uint64
	rtpIngressDropped   map[string]uint64
	rtpOutOfOrder       map[string]uint64
	rtpPacketsDiscarded map[string]uint64
	rtpFramesAssembled  map[string]uint64
	processing          map[string]*histogram
	aiDuration          map[string]*histogram
	stageDuration       map[string]*histogram
}

func New() *Registry {
	return &Registry{
		aiFallbackLatched:   make(map[string]uint64),
		aiFallbackFrames:    make(map[string]uint64),
		aiInputPausedFrames: make(map[string]uint64),
		aiTargetSessions:    make(map[string]uint64),
		framesReceived:      make(map[string]uint64),
		framesProcessed:     make(map[string]uint64),
		framesDropped:       make(map[string]uint64),
		frameFailures:       make(map[string]uint64),
		rtpPacketsReceived:  make(map[string]uint64),
		rtpSequenceGaps:     make(map[string]uint64),
		rtpPacketsRecovered: make(map[string]uint64),
		rtpIngressDropped:   make(map[string]uint64),
		rtpOutOfOrder:       make(map[string]uint64),
		rtpPacketsDiscarded: make(map[string]uint64),
		rtpFramesAssembled:  make(map[string]uint64),
		processing:          make(map[string]*histogram),
		aiDuration:          make(map[string]*histogram),
		stageDuration:       make(map[string]*histogram),
		processTree:         newProcessTreeTracker(),
	}
}

func (r *Registry) SetActiveSessions(value int) { r.activeSessions.Store(int64(value)) }
func (r *Registry) IncConnections()             { r.connections.Add(1) }
func (r *Registry) IncConnectionFailures()      { r.connectionFailure.Add(1) }
func (r *Registry) IncReconnects()              { r.reconnects.Add(1) }
func (r *Registry) IncPeerRecoveryStarted()     { r.peerRecoveryStart.Add(1) }
func (r *Registry) IncPeerRecoverySucceeded()   { r.peerRecoveryOK.Add(1) }
func (r *Registry) IncPeerRecoveryExhausted()   { r.peerRecoveryEnd.Add(1) }
func (r *Registry) IncSessionsRejected()        { r.sessionsRejected.Add(1) }
func (r *Registry) AddQueue(delta int64)        { r.queueSize.Add(delta) }

func (r *Registry) IncEgressReconnect()             { r.egressReconnects.Add(1) }
func (r *Registry) IncEgressReconnectExhausted()    { r.egressExhausted.Add(1) }
func (r *Registry) IncEgressReconnectInputTimeout() { r.egressInputTimed.Add(1) }
func (r *Registry) IncEgressFrameDropped()          { r.egressDropped.Add(1) }
func (r *Registry) SetEgressDupFrames(value int64)  { r.egressDupFrames.Store(value) }
func (r *Registry) SetEgressDropFrames(value int64) { r.egressDropFrames.Store(value) }

func (r *Registry) IncAudioSampleWritten() { r.audioSamplesOut.Add(1) }
func (r *Registry) IncAudioSampleDropped() { r.audioSamplesDrop.Add(1) }

func (r *Registry) IncAIFallbackLatched(mode string) {
	r.increment(r.aiFallbackLatched, mode)
}

func (r *Registry) IncAIFallbackFrame(mode string) {
	r.increment(r.aiFallbackFrames, mode)
}

// IncAIInputPausedFrame은 방송 일시 중지 중 AI worker로 보내지 않고 버린
// 카메라 프레임 수를 기록한다.
func (r *Registry) IncAIInputPausedFrame(mode string) {
	r.increment(r.aiInputPausedFrames, mode)
}

func (r *Registry) IncAITargetSession(target string) {
	r.increment(r.aiTargetSessions, target)
}

func (r *Registry) IncFrameReceived(mode string) {
	r.increment(r.framesReceived, mode)
}

func (r *Registry) IncFrameProcessed(mode string) {
	r.increment(r.framesProcessed, mode)
}

func (r *Registry) IncFrameDropped(mode string) {
	r.increment(r.framesDropped, mode)
}

func (r *Registry) IncFrameFailure(mode string) {
	r.increment(r.frameFailures, mode)
}

func (r *Registry) IncRTPPacketsReceived(mode string) {
	r.increment(r.rtpPacketsReceived, mode)
}

func (r *Registry) AddRTPSequenceGaps(mode string, count uint64) {
	r.incrementBy(r.rtpSequenceGaps, mode, count)
}

func (r *Registry) IncRTPPacketsRecovered(mode string) {
	r.increment(r.rtpPacketsRecovered, mode)
}

func (r *Registry) IncRTPIngressPacketDropped(mode string) {
	r.increment(r.rtpIngressDropped, mode)
}

func (r *Registry) IncRTPOutOfOrder(mode string) {
	r.increment(r.rtpOutOfOrder, mode)
}

func (r *Registry) AddRTPPacketsDiscarded(mode string, count uint64) {
	r.incrementBy(r.rtpPacketsDiscarded, mode, count)
}

func (r *Registry) IncRTPFramesAssembled(mode string) {
	r.increment(r.rtpFramesAssembled, mode)
}

func (r *Registry) ObserveProcessing(mode string, duration time.Duration) {
	r.observe(r.processing, mode, duration)
}

func (r *Registry) ObserveAI(mode string, duration time.Duration) {
	r.observe(r.aiDuration, mode, duration)
}

func (r *Registry) ObserveStage(stage string, duration time.Duration) {
	r.observe(r.stageDuration, stage, duration)
}

func (r *Registry) increment(values map[string]uint64, label string) {
	r.mu.Lock()
	values[label]++
	r.mu.Unlock()
}

func (r *Registry) incrementBy(values map[string]uint64, label string, count uint64) {
	r.mu.Lock()
	values[label] += count
	r.mu.Unlock()
}

func (r *Registry) observe(values map[string]*histogram, label string, duration time.Duration) {
	seconds := duration.Seconds()
	r.mu.Lock()
	h := values[label]
	if h == nil {
		h = &histogram{buckets: make([]uint64, len(defaultBuckets)+1)}
		values[label] = h
	}
	h.count++
	h.sum += seconds
	index := sort.SearchFloat64s(defaultBuckets, seconds)
	for i := index; i < len(h.buckets); i++ {
		h.buckets[i]++
	}
	r.mu.Unlock()
}

func (r *Registry) WritePrometheus(w io.Writer) {
	processCPUSeconds, processRSSBytes := readProcessStats()
	processTreeCPUSeconds, processTreeRSSBytes := r.processTreeStats(processCPUSeconds, processRSSBytes)
	writeFloatCounter(w, "process_cpu_seconds_total", "Total user and system CPU time spent in seconds.", processCPUSeconds)
	writeGauge(w, "process_resident_memory_bytes", "Resident memory size in bytes.", int64(processRSSBytes))
	writeFloatCounter(w, "innolive_process_tree_cpu_seconds_total", "Total CPU time spent by the server and its FFmpeg child processes.", processTreeCPUSeconds)
	writeGauge(w, "innolive_process_tree_resident_memory_bytes", "Resident memory used by the server and its active FFmpeg child processes.", int64(processTreeRSSBytes))
	writeGauge(w, "innolive_active_sessions", "Number of active WebRTC sessions.", r.activeSessions.Load())
	writeCounter(w, "innolive_connection_total", "Number of WebRTC sessions created.", r.connections.Load())
	writeCounter(w, "innolive_connection_failures_total", "Number of WebRTC connections that reached a failed state.", r.connectionFailure.Load())
	writeCounter(w, "innolive_reconnect_total", "Number of WebRTC sessions that recovered after disconnecting.", r.reconnects.Load())
	writeCounter(w, "innolive_peer_recovery_started_total", "Number of WebRTC peer recovery windows started after network loss.", r.peerRecoveryStart.Load())
	writeCounter(w, "innolive_peer_recovery_succeeded_total", "Number of WebRTC peer recovery windows completed after a processed camera frame returned.", r.peerRecoveryOK.Load())
	writeCounter(w, "innolive_peer_recovery_exhausted_total", "Number of WebRTC sessions ended after their network recovery window expired.", r.peerRecoveryEnd.Load())
	writeCounter(w, "innolive_sessions_rejected_total", "Number of session creations rejected by the MAX_SESSIONS admission control.", r.sessionsRejected.Load())
	writeGauge(w, "innolive_processing_queue_size", "Number of complete video frames waiting for processing.", r.queueSize.Load())
	writeCounter(w, "innolive_egress_reconnect_total", "Number of times the RTMP egress FFmpeg process was restarted.", r.egressReconnects.Load())
	writeCounter(w, "innolive_egress_reconnect_exhausted_total", "Number of RTMP egresses stopped after their reconnect retry budget was exhausted.", r.egressExhausted.Load())
	writeCounter(w, "innolive_egress_reconnect_input_timeout_total", "Number of RTMP egresses stopped while waiting too long for reconfiguration input frames.", r.egressInputTimed.Load())
	writeCounter(w, "innolive_egress_frames_dropped_total", "Number of frames the RTMP egress discarded (full queue, invalid frame, or disconnected).", r.egressDropped.Load())
	writeGauge(w, "innolive_egress_dup_frames", "Frames duplicated by the egress CFR pacing in the current FFmpeg process.", r.egressDupFrames.Load())
	writeGauge(w, "innolive_egress_drop_frames", "Frames dropped by the egress CFR pacing in the current FFmpeg process.", r.egressDropFrames.Load())
	writeCounter(w, "innolive_audio_samples_written_total", "Number of Opus samples written to the RTMP egress audio pipe.", r.audioSamplesOut.Load())
	writeCounter(w, "innolive_audio_samples_dropped_total", "Number of Opus samples dropped before the audio pipe (full queue, detached, non-monotonic, or pipe error).", r.audioSamplesDrop.Load())

	r.mu.RLock()
	defer r.mu.RUnlock()
	writeLabeledCountersWithKey(w, "innolive_ai_fallback_latched_total", "Number of sessions that latched the fail-closed blackout after an AI failure.", "mode", r.aiFallbackLatched)
	writeLabeledCountersWithKey(w, "innolive_ai_fallback_frames_total", "Number of blackout frames emitted by latched sessions.", "mode", r.aiFallbackFrames)
	writeLabeledCountersWithKey(w, "innolive_ai_input_paused_frames_total", "Number of camera frames discarded before AI processing while a broadcast is paused.", "mode", r.aiInputPausedFrames)
	writeLabeledCountersWithKey(w, "innolive_ai_target_sessions_total", "Number of sessions assigned to each AI worker target.", "target", r.aiTargetSessions)
	writeLabeledCounters(w, "innolive_frame_received_total", "Number of complete video frames received for processing.", r.framesReceived)
	writeLabeledCounters(w, "innolive_frame_processed_total", "Number of processed video frames returned to WebRTC.", r.framesProcessed)
	writeLabeledCounters(w, "innolive_frame_dropped_total", "Number of stale decoded frames discarded by the processing queue.", r.framesDropped)
	writeLabeledCounters(w, "innolive_frame_failures_total", "Number of frame processing failures.", r.frameFailures)
	writeLabeledCounters(w, "innolive_rtp_packets_received_total", "Number of RTP video packets read by the server.", r.rtpPacketsReceived)
	writeLabeledCounters(w, "innolive_rtp_sequence_gaps_total", "Number of RTP sequence numbers initially observed as missing.", r.rtpSequenceGaps)
	writeLabeledCounters(w, "innolive_rtp_packets_recovered_total", "Number of previously missing RTP packets received within the reorder window.", r.rtpPacketsRecovered)
	writeLabeledCounters(w, "innolive_rtp_ingress_packets_dropped_total", "Number of oldest RTP packets discarded because the ingress queue was full.", r.rtpIngressDropped)
	writeLabeledCounters(w, "innolive_rtp_out_of_order_total", "Number of duplicate or late RTP packets not tracked as missing.", r.rtpOutOfOrder)
	writeLabeledCounters(w, "innolive_rtp_packets_discarded_total", "Number of RTP packets discarded while assembling complete video frames.", r.rtpPacketsDiscarded)
	writeLabeledCounters(w, "innolive_rtp_frames_assembled_total", "Number of complete VP8 frames assembled from RTP packets.", r.rtpFramesAssembled)
	writeHistograms(w, "innolive_frame_processing_duration_seconds", "Time spent processing one complete video frame.", "mode", r.processing)
	writeHistograms(w, "innolive_ai_duration_seconds", "Round-trip time spent in the external AI service.", "mode", r.aiDuration)
	writeHistograms(w, "innolive_ai_stage_duration_seconds", "Time spent in a frame processing stage.", "stage", r.stageDuration)
	writeSingleHistogram(w, "innolive_decode_duration_seconds", "Time spent decoding VP8 into a JPEG image.", r.stageDuration["decode"])
	writeSingleHistogram(w, "innolive_encode_duration_seconds", "Time spent encoding a processed JPEG image into VP8.", r.stageDuration["encode"])
}

func writeGauge(w io.Writer, name, help string, value int64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, value)
}

func writeCounter(w io.Writer, name, help string, value uint64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, value)
}

func writeFloatCounter(w io.Writer, name, help string, value float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %s\n", name, help, name, name, formatFloat(value))
}

func writeLabeledCounters(w io.Writer, name, help string, values map[string]uint64) {
	writeLabeledCountersWithKey(w, name, help, "mode", values)
}

func writeLabeledCountersWithKey(w io.Writer, name, help, labelKey string, values map[string]uint64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	for _, label := range sortedKeys(values) {
		fmt.Fprintf(w, "%s{%s=%q} %d\n", name, labelKey, label, values[label])
	}
}

func writeHistograms(w io.Writer, name, help, labelName string, values map[string]*histogram) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	for _, label := range sortedKeys(values) {
		h := values[label]
		for index, upper := range defaultBuckets {
			fmt.Fprintf(w, "%s_bucket{%s=%q,le=%q} %d\n", name, labelName, label, formatFloat(upper), h.buckets[index])
		}
		fmt.Fprintf(w, "%s_bucket{%s=%q,le=\"+Inf\"} %d\n", name, labelName, label, h.buckets[len(h.buckets)-1])
		fmt.Fprintf(w, "%s_sum{%s=%q} %s\n", name, labelName, label, formatFloat(h.sum))
		fmt.Fprintf(w, "%s_count{%s=%q} %d\n", name, labelName, label, h.count)
	}
}

func writeSingleHistogram(w io.Writer, name, help string, h *histogram) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	if h == nil {
		return
	}
	for index, upper := range defaultBuckets {
		fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", name, formatFloat(upper), h.buckets[index])
	}
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, h.buckets[len(h.buckets)-1])
	fmt.Fprintf(w, "%s_sum %s\n", name, formatFloat(h.sum))
	fmt.Fprintf(w, "%s_count %d\n", name, h.count)
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func formatFloat(value float64) string {
	if math.IsInf(value, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}
