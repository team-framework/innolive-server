package media

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

const (
	// egressQueueSize bounds the blur → egress channel. When full the oldest
	// frame is dropped so the blur stage never blocks on a slow RTMP peer.
	egressQueueSize = 5
	// egressMeasureFrames is how many leading frames are buffered to derive
	// the source frame rate from their RTP timestamps before the first spawn.
	egressMeasureFrames = 30
	egressDefaultFPS    = 30
	egressVideoBitrate  = "2500k"
	egressAudioBitrate  = "128k"
	egressBackoffMin    = time.Second
	egressBackoffMax    = 30 * time.Second
	// egressStableFrames is how many frames a process must accept before a
	// later failure resets the reconnect backoff to its minimum.
	egressStableFrames = 300
)

// RTMPEgress pushes processed (blurred) frames to an RTMP endpoint through a
// dedicated FFmpeg child, following the FFmpegTranscoder process pattern. The
// audio track is generated silence: the WebRTC ingest discards audio tracks,
// so no real audio ever reaches this stage, and YouTube rejects streams
// without an audio track.
type RTMPEgress struct {
	transcoder *FFmpegTranscoder
	logger     *slog.Logger
	metrics    *metrics.Registry
	wireFormat config.WireFormat
	outputURL  string
	input      chan frame
}

func NewRTMPEgress(path string, logger *slog.Logger, registry *metrics.Registry, options TranscoderOptions, outputURL string) *RTMPEgress {
	if options.WireFormat == "" {
		options.WireFormat = config.WireFormatJPEG
	}
	return &RTMPEgress{
		transcoder: NewFFmpegTranscoder(path, logger, registry, options),
		logger:     logger.With("ffmpeg_role", "egress"),
		metrics:    registry,
		wireFormat: options.WireFormat,
		outputURL:  outputURL,
		input:      make(chan frame, egressQueueSize),
	}
}

// Enqueue hands a processed frame to the egress without ever blocking the
// caller: when the queue is full the oldest frame is discarded first.
func (e *RTMPEgress) Enqueue(item frame) {
	select {
	case e.input <- item:
		return
	default:
	}
	select {
	case <-e.input:
		e.metrics.IncEgressFrameDropped()
	default:
	}
	select {
	case e.input <- item:
	default:
		e.metrics.IncEgressFrameDropped()
	}
}

// Run owns the single writer goroutine: it measures the source frame rate,
// spawns the FFmpeg child, feeds it frames, and reconnects with exponential
// backoff when the child dies or the RTMP write path fails. Frames arriving
// while disconnected are dropped, never buffered.
func (e *RTMPEgress) Run(ctx context.Context) {
	startup, ok := e.collectStartupFrames(ctx)
	if !ok {
		return
	}
	width, height := startup[0].width, startup[0].height
	fps := measureFPS(startup)
	e.logger.Info("starting RTMP egress",
		"url", maskStreamKey(e.outputURL), "width", width, "height", height, "fps", fps)

	backoff := egressBackoffMin
	pending := startup
	for ctx.Err() == nil {
		process, err := e.start(ctx, width, height, fps)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.logger.Error("start RTMP egress FFmpeg failed", "error", err)
			e.waitBackoff(ctx, &backoff)
			continue
		}
		written, err := e.writeFrames(ctx, process, pending)
		pending = nil
		process.close()
		if ctx.Err() != nil {
			return
		}
		if written >= egressStableFrames {
			backoff = egressBackoffMin
		}
		e.metrics.IncEgressReconnect()
		e.logger.Warn("RTMP egress disconnected; reconnecting",
			"error", err, "frames_written", written, "backoff", backoff)
		e.waitBackoff(ctx, &backoff)
	}
}

func (e *RTMPEgress) collectStartupFrames(ctx context.Context) ([]frame, bool) {
	startup := make([]frame, 0, egressMeasureFrames)
	for len(startup) < egressMeasureFrames {
		select {
		case <-ctx.Done():
			return nil, false
		case item := <-e.input:
			startup = append(startup, item)
		}
	}
	return startup, true
}

func (e *RTMPEgress) writeFrames(ctx context.Context, process *ffmpegProcess, pending []frame) (int, error) {
	written := 0
	write := func(item frame) error {
		if !e.validFrame(item) {
			e.metrics.IncEgressFrameDropped()
			return nil
		}
		if _, err := process.stdin.Write(item.data); err != nil {
			return err
		}
		written++
		return nil
	}
	for _, item := range pending {
		if err := write(item); err != nil {
			return written, err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		case item := <-e.input:
			if err := write(item); err != nil {
				return written, err
			}
		}
	}
}

func (e *RTMPEgress) validFrame(item frame) bool {
	if e.wireFormat == config.WireFormatRaw {
		return len(item.data) == rawFrameSize(item.width, item.height)
	}
	return isJPEG(item.data)
}

// waitBackoff sleeps for the current backoff while draining (dropping) frames
// so the queue holds fresh frames when the process comes back, then doubles
// the backoff up to its cap.
func (e *RTMPEgress) waitBackoff(ctx context.Context, backoff *time.Duration) {
	timer := time.NewTimer(*backoff)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.input:
			e.metrics.IncEgressFrameDropped()
		case <-timer.C:
			*backoff *= 2
			if *backoff > egressBackoffMax {
				*backoff = egressBackoffMax
			}
			return
		}
	}
}

func (e *RTMPEgress) start(ctx context.Context, width, height uint16, fps int) (*ffmpegProcess, error) {
	arguments := []string{
		"-hide_banner", "-loglevel", "error", "-nostats", "-progress", "pipe:2", "-y",
		"-thread_queue_size", "512", "-use_wallclock_as_timestamps", "1",
	}
	if e.wireFormat == config.WireFormatRaw {
		arguments = append(arguments,
			"-f", "rawvideo", "-pixel_format", "yuv420p",
			"-video_size", fmt.Sprintf("%dx%d", width, height),
			"-framerate", strconv.Itoa(fps), "-i", "pipe:0",
		)
	} else {
		arguments = append(arguments,
			"-f", "image2pipe", "-vcodec", "mjpeg",
			"-framerate", strconv.Itoa(fps), "-i", "pipe:0",
		)
	}
	arguments = append(arguments,
		"-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo",
		"-map", "0:v", "-map", "1:a",
	)
	if e.wireFormat != config.WireFormatRaw {
		// MJPEG decodes to full-range yuvj420p; compress to limited range so
		// the FLV stream carries plain yuv420p.
		arguments = append(arguments, "-vf", "scale=out_range=tv")
	}
	arguments = append(arguments,
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency",
		"-pix_fmt", "yuv420p", "-profile:v", "main",
		"-b:v", egressVideoBitrate, "-maxrate", egressVideoBitrate, "-bufsize", egressVideoBitrate,
		"-g", strconv.Itoa(fps*2), "-bf", "0",
		"-r", strconv.Itoa(fps), "-fps_mode", "cfr",
		"-c:a", "aac", "-b:a", egressAudioBitrate, "-ar", "44100",
		"-shortest",
		"-f", "flv", e.outputURL,
	)
	process, err := e.transcoder.startFFmpeg(ctx, "egress", e.handleStderrLine, arguments...)
	if err != nil {
		return nil, fmt.Errorf("start FFmpeg RTMP egress: %w", err)
	}
	return process, nil
}

// handleStderrLine splits FFmpeg's -progress key=value stream (carrying the
// dup/drop frame counters produced by -fps_mode cfr) from genuine errors.
func (e *RTMPEgress) handleStderrLine(line string) {
	key, value, found := strings.Cut(line, "=")
	if found && isProgressKey(key) {
		switch key {
		case "dup_frames":
			if parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				e.metrics.SetEgressDupFrames(parsed)
			}
		case "drop_frames":
			if parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				e.metrics.SetEgressDropFrames(parsed)
			}
		}
		return
	}
	// FFmpeg echoes the output URL (which carries the stream key) into its
	// own error messages; mask it before it reaches the logs.
	e.logger.Warn("FFmpeg reported an error", "message", strings.ReplaceAll(line, e.outputURL, maskStreamKey(e.outputURL)))
}

func isProgressKey(key string) bool {
	switch key {
	case "frame", "fps", "bitrate", "total_size", "out_time_us", "out_time_ms",
		"out_time", "dup_frames", "drop_frames", "speed", "progress":
		return true
	}
	return strings.HasPrefix(key, "stream_")
}

// measureFPS derives the source frame rate from the RTP timestamp span
// (90 kHz clock) of the buffered startup frames. uint32 subtraction handles
// timestamp wrap-around.
func measureFPS(frames []frame) int {
	if len(frames) < 2 {
		return egressDefaultFPS
	}
	delta := frames[len(frames)-1].timestamp - frames[0].timestamp
	if delta == 0 {
		return egressDefaultFPS
	}
	fps := int(math.Round(float64(len(frames)-1) * videoClockRate / float64(delta)))
	if fps < 1 || fps > 120 {
		return egressDefaultFPS
	}
	return fps
}

// maskStreamKey hides the final path segment (the stream key) of an RTMP URL
// so it never reaches the logs.
func maskStreamKey(url string) string {
	index := strings.LastIndex(url, "/")
	if index < 0 || index == len(url)-1 {
		return url
	}
	return url[:index+1] + "****"
}
