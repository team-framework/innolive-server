package media

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

const (
	maxEncodedFrameSize = 20 << 20
	// ffmpegShutdownGrace bounds how long teardown waits for FFmpeg to exit
	// on its own (stdin EOF or SIGINT) before falling back to SIGKILL.
	ffmpegShutdownGrace = 2 * time.Second
)

var ErrKeyframeRequired = errors.New("a VP8 keyframe is required to initialize the decoder")

type commandFactory func(ctx context.Context, name string, arguments ...string) *exec.Cmd

// TranscoderOptions carries the per-process resource policy shared by every
// session's FFmpeg pair.
type TranscoderOptions struct {
	// Gate bounds concurrent FFmpeg process starts. nil means unlimited.
	Gate *SpawnGate
	// EncoderThreads caps libvpx encoder threads; 0 lets FFmpeg auto-detect.
	EncoderThreads int
	// WireFormat selects the decoded-frame format exchanged with the AI
	// boundary: JPEG (default) or raw yuv420p.
	WireFormat config.WireFormat
}

type FFmpegTranscoder struct {
	path       string
	logger     *slog.Logger
	metrics    *metrics.Registry
	options    TranscoderOptions
	newCommand commandFactory
}

type ffmpegProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	wait    sync.Once
	waitErr error
	metrics *metrics.Registry
}

func NewFFmpegTranscoder(path string, logger *slog.Logger, registry *metrics.Registry, options TranscoderOptions) *FFmpegTranscoder {
	if options.WireFormat == "" {
		options.WireFormat = config.WireFormatJPEG
	}
	return &FFmpegTranscoder{
		path:       path,
		logger:     logger,
		metrics:    registry,
		options:    options,
		newCommand: exec.CommandContext,
	}
}

func (t *FFmpegTranscoder) DecodeStream(ctx context.Context, input <-chan frame, output chan<- frame) error {
	first, ok := <-input
	if !ok {
		return nil
	}
	width, height, err := vp8KeyframeDimensions(first.data)
	for errors.Is(err, ErrKeyframeRequired) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case first, ok = <-input:
			if !ok {
				return nil
			}
			width, height, err = vp8KeyframeDimensions(first.data)
		}
	}
	if err != nil {
		return err
	}
	process, err := t.startDecoder(ctx, width, height)
	if err != nil {
		return err
	}
	defer process.close()

	metadata := make(chan frame, 64)
	writeError := make(chan error, 1)
	go func() {
		defer close(metadata)
		defer process.stdin.Close()
		index := uint64(0)
		write := func(item frame) error {
			item.width, item.height = width, height
			if err := writeIVFFrame(process.stdin, item.data, index); err != nil {
				return err
			}
			index++
			select {
			case metadata <- item:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := write(first); err != nil {
			writeError <- err
			return
		}
		for {
			select {
			case <-ctx.Done():
				writeError <- ctx.Err()
				return
			case item, ok := <-input:
				if !ok {
					writeError <- nil
					return
				}
				if err := write(item); err != nil {
					writeError <- err
					return
				}
			}
		}
	}()

	rawSize := rawFrameSize(width, height)
	for item := range metadata {
		var decoded []byte
		if t.options.WireFormat == config.WireFormatRaw {
			decoded, err = readRawFrame(process.stdout, rawSize)
		} else {
			decoded, err = readJPEG(process.stdout)
		}
		if err != nil {
			return fmt.Errorf("read decoded frame from FFmpeg decoder: %w", err)
		}
		item.data = decoded
		select {
		case output <- item:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := <-writeError; err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("write VP8 stream to FFmpeg decoder: %w", err)
	}
	return nil
}

func (t *FFmpegTranscoder) EncodeStream(ctx context.Context, input <-chan frame, output chan<- frame) error {
	// The encoder process is spawned only once the first processed frame
	// exists, mirroring the decoder's keyframe wait. This keeps a cold spike
	// of N sessions from forking N encoders before any media flows.
	var first frame
	select {
	case <-ctx.Done():
		return ctx.Err()
	case item, ok := <-input:
		if !ok {
			return nil
		}
		first = item
	}
	process, err := t.startEncoder(ctx, first.width, first.height)
	if err != nil {
		return err
	}
	defer process.close()

	expectedRawSize := rawFrameSize(first.width, first.height)
	metadata := make(chan frame, 64)
	writeError := make(chan error, 1)
	go func() {
		defer close(metadata)
		defer process.stdin.Close()
		write := func(item frame) error {
			if err := t.validateEncoderInput(item.data, expectedRawSize); err != nil {
				return err
			}
			if _, err := process.stdin.Write(item.data); err != nil {
				return err
			}
			select {
			case metadata <- item:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := write(first); err != nil {
			writeError <- err
			return
		}
		for {
			select {
			case <-ctx.Done():
				writeError <- ctx.Err()
				return
			case item, ok := <-input:
				if !ok {
					writeError <- nil
					return
				}
				if err := write(item); err != nil {
					writeError <- err
					return
				}
			}
		}
	}()

	headerRead := false
	for item := range metadata {
		if !headerRead {
			header := make([]byte, 32)
			if _, err := io.ReadFull(process.stdout, header); err != nil {
				return fmt.Errorf("read FFmpeg encoder IVF header: %w", err)
			}
			if string(header[:4]) != "DKIF" || string(header[8:12]) != "VP80" {
				return errors.New("FFmpeg encoder returned an invalid VP8 IVF stream")
			}
			headerRead = true
		}
		encoded, _, err := readIVFFrame(process.stdout)
		if err != nil {
			return fmt.Errorf("read VP8 frame from FFmpeg encoder: %w", err)
		}
		item.data = encoded
		select {
		case output <- item:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := <-writeError; err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("write frame stream to FFmpeg encoder: %w", err)
	}
	return nil
}

func (t *FFmpegTranscoder) validateEncoderInput(data []byte, expectedRawSize int) error {
	if t.options.WireFormat == config.WireFormatRaw {
		if len(data) != expectedRawSize {
			return fmt.Errorf("AI response is not a raw yuv420p frame: got %d bytes, want %d", len(data), expectedRawSize)
		}
		return nil
	}
	if !isJPEG(data) {
		return errors.New("AI response is not a complete JPEG image")
	}
	return nil
}

func (t *FFmpegTranscoder) startDecoder(ctx context.Context, width, height uint16) (*ffmpegProcess, error) {
	arguments := []string{
		"-hide_banner", "-loglevel", "error",
		"-probesize", "32", "-analyzeduration", "0", "-fpsprobesize", "0", "-threads", "1",
		"-f", "ivf", "-blocksize", "1024", "-i", "pipe:0",
	}
	if t.options.WireFormat == config.WireFormatRaw {
		arguments = append(arguments, "-an", "-f", "rawvideo", "-pix_fmt", "yuv420p", "-flush_packets", "1", "-blocksize", "1024")
	} else {
		arguments = append(arguments, "-an", "-strict", "unofficial", "-f", "image2pipe", "-vcodec", "mjpeg", "-q:v", "3", "-pix_fmt", "yuvj420p", "-flush_packets", "1", "-blocksize", "1024")
	}
	arguments = append(arguments, "pipe:1")
	process, err := t.startFFmpeg(ctx, "decoder", arguments...)
	if err != nil {
		return nil, fmt.Errorf("start FFmpeg VP8 decoder: %w", err)
	}
	if _, err := process.stdin.Write(ivfHeader(width, height, 30, 1)); err != nil {
		process.close()
		return nil, fmt.Errorf("write FFmpeg decoder IVF header: %w", err)
	}
	return process, nil
}

func (t *FFmpegTranscoder) startEncoder(ctx context.Context, width, height uint16) (*ffmpegProcess, error) {
	arguments := []string{
		"-hide_banner", "-loglevel", "error",
		"-probesize", "32", "-analyzeduration", "0", "-fpsprobesize", "0",
	}
	if t.options.WireFormat == config.WireFormatRaw {
		arguments = append(arguments,
			"-f", "rawvideo", "-pixel_format", "yuv420p",
			"-video_size", fmt.Sprintf("%dx%d", width, height),
			"-framerate", "30", "-blocksize", "1024", "-i", "pipe:0",
		)
	} else {
		arguments = append(arguments, "-f", "image2pipe", "-vcodec", "mjpeg", "-framerate", "30", "-blocksize", "1024", "-i", "pipe:0")
	}
	if t.options.EncoderThreads > 0 {
		arguments = append(arguments, "-threads", strconv.Itoa(t.options.EncoderThreads))
	}
	arguments = append(arguments,
		"-an", "-c:v", "libvpx", "-deadline", "realtime", "-cpu-used", "8",
		"-lag-in-frames", "0", "-auto-alt-ref", "0", "-g", "30", "-b:v", "2M",
		"-pix_fmt", "yuv420p", "-flush_packets", "1", "-f", "ivf", "-blocksize", "1024", "pipe:1",
	)
	process, err := t.startFFmpeg(ctx, "encoder", arguments...)
	if err != nil {
		return nil, fmt.Errorf("start FFmpeg VP8 encoder: %w", err)
	}
	return process, nil
}

func (t *FFmpegTranscoder) startFFmpeg(ctx context.Context, role string, arguments ...string) (*ffmpegProcess, error) {
	logger := t.logger.With("ffmpeg_role", role)
	cmd := t.newCommand(ctx, t.path, arguments...)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(os.Interrupt)
	}
	cmd.WaitDelay = ffmpegShutdownGrace
	// Acquire the gate before creating any pipes: a start that is cancelled
	// while queued must not have allocated file descriptors (exec only
	// closes them via Start's own cleanup, which never runs on this path).
	if err := t.options.Gate.Acquire(ctx); err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.options.Gate.Release()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.options.Gate.Release()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.options.Gate.Release()
		return nil, err
	}
	err = cmd.Start()
	t.options.Gate.Release()
	if err != nil {
		return nil, err
	}
	if t.metrics != nil {
		t.metrics.RegisterChildProcess(cmd.Process.Pid)
	}
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			logger.Warn("FFmpeg reported an error", "message", scanner.Text())
		}
	}()
	return &ffmpegProcess{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReaderSize(stdout, 1<<20),
		metrics: t.metrics,
	}, nil
}

func (p *ffmpegProcess) close() {
	p.wait.Do(func() {
		_ = p.stdin.Close()
		done := make(chan error, 1)
		go func() { done <- p.cmd.Wait() }()
		timer := time.NewTimer(ffmpegShutdownGrace)
		defer timer.Stop()
		select {
		case p.waitErr = <-done:
		case <-timer.C:
			if p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
			p.waitErr = <-done
		}
		if p.metrics != nil && p.cmd.Process != nil {
			cpuSeconds := 0.0
			if p.cmd.ProcessState != nil {
				cpuSeconds = (p.cmd.ProcessState.UserTime() + p.cmd.ProcessState.SystemTime()).Seconds()
			}
			p.metrics.CompleteChildProcess(p.cmd.Process.Pid, cpuSeconds)
		}
	})
}

func vp8KeyframeDimensions(frame []byte) (uint16, uint16, error) {
	if len(frame) < 10 || frame[0]&1 != 0 {
		return 0, 0, ErrKeyframeRequired
	}
	if frame[3] != 0x9d || frame[4] != 0x01 || frame[5] != 0x2a {
		return 0, 0, errors.New("invalid VP8 keyframe start code")
	}
	width := binary.LittleEndian.Uint16(frame[6:8]) & 0x3fff
	height := binary.LittleEndian.Uint16(frame[8:10]) & 0x3fff
	if width == 0 || height == 0 {
		return 0, 0, errors.New("invalid VP8 keyframe dimensions")
	}
	return width, height, nil
}

func ivfHeader(width, height uint16, frameRate, timeScale uint32) []byte {
	header := make([]byte, 32)
	copy(header[0:4], "DKIF")
	binary.LittleEndian.PutUint16(header[6:8], 32)
	copy(header[8:12], "VP80")
	binary.LittleEndian.PutUint16(header[12:14], width)
	binary.LittleEndian.PutUint16(header[14:16], height)
	binary.LittleEndian.PutUint32(header[16:20], frameRate)
	binary.LittleEndian.PutUint32(header[20:24], timeScale)
	binary.LittleEndian.PutUint32(header[24:28], ^uint32(0))
	return header
}

func writeIVFFrame(writer io.Writer, frame []byte, timestamp uint64) error {
	header := make([]byte, 12)
	binary.LittleEndian.PutUint32(header[0:4], uint32(len(frame)))
	binary.LittleEndian.PutUint64(header[4:12], timestamp)
	if _, err := writer.Write(header); err != nil {
		return err
	}
	_, err := writer.Write(frame)
	return err
}

func readIVFFrame(reader io.Reader) ([]byte, uint64, error) {
	header := make([]byte, 12)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, 0, err
	}
	size := int(binary.LittleEndian.Uint32(header[:4]))
	if size <= 0 || size > maxEncodedFrameSize {
		return nil, 0, fmt.Errorf("invalid IVF frame size %d", size)
	}
	frame := make([]byte, size)
	if _, err := io.ReadFull(reader, frame); err != nil {
		return nil, 0, err
	}
	return frame, binary.LittleEndian.Uint64(header[4:12]), nil
}

// rawFrameSize returns the byte size of one yuv420p frame, with chroma
// planes rounded up for odd dimensions the way FFmpeg emits them.
func rawFrameSize(width, height uint16) int {
	w, h := int(width), int(height)
	return w*h + 2*((w+1)/2)*((h+1)/2)
}

func readRawFrame(reader *bufio.Reader, size int) ([]byte, error) {
	if size <= 0 {
		return nil, errors.New("invalid raw frame size")
	}
	buffer := make([]byte, size)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return nil, err
	}
	return buffer, nil
}

// readJPEG extracts one JPEG image from a byte stream by scanning buffered
// windows for the EOI marker. The 0xFFD9 sequence cannot occur inside
// entropy-coded data (byte stuffing escapes 0xFF as 0xFF00), so the first
// occurrence terminates the image.
func readJPEG(reader *bufio.Reader) ([]byte, error) {
	start, err := reader.Peek(2)
	if err != nil {
		return nil, err
	}
	if start[0] != 0xff || start[1] != 0xd8 {
		return nil, errors.New("FFmpeg decoder output is not JPEG")
	}
	frame := make([]byte, 0, 256<<10)
	for {
		if len(frame) > maxEncodedFrameSize {
			return nil, errors.New("JPEG frame exceeds maximum size")
		}
		if _, err := reader.Peek(1); err != nil {
			return nil, err
		}
		chunk, err := reader.Peek(reader.Buffered())
		if err != nil {
			return nil, err
		}
		if len(frame) > 0 && frame[len(frame)-1] == 0xff && chunk[0] == 0xd9 {
			frame = append(frame, 0xd9)
			if _, err := reader.Discard(1); err != nil {
				return nil, err
			}
			return frame, nil
		}
		if index := indexEOI(chunk); index >= 0 {
			frame = append(frame, chunk[:index+2]...)
			if _, err := reader.Discard(index + 2); err != nil {
				return nil, err
			}
			return frame, nil
		}
		frame = append(frame, chunk...)
		if _, err := reader.Discard(len(chunk)); err != nil {
			return nil, err
		}
	}
}

func indexEOI(chunk []byte) int {
	offset := 0
	for {
		index := bytes.IndexByte(chunk[offset:], 0xff)
		if index < 0 || offset+index+1 >= len(chunk) {
			return -1
		}
		if chunk[offset+index+1] == 0xd9 {
			return offset + index
		}
		offset += index + 1
	}
}

func isJPEG(frame []byte) bool {
	return len(frame) >= 4 && frame[0] == 0xff && frame[1] == 0xd8 && frame[len(frame)-2] == 0xff && frame[len(frame)-1] == 0xd9
}
