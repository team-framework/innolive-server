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
	"syscall"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

const (
	maxEncodedFrameSize = 20 << 20
	// ffmpegShutdownGrace는 teardown이 FFmpeg의 자체 종료(stdin EOF 또는
	// SIGINT)를 얼마나 기다린 뒤 SIGKILL로 넘어갈지를 정한다.
	ffmpegShutdownGrace = 2 * time.Second
)

var ErrKeyframeRequired = errors.New("a video keyframe is required to initialize the decoder")

// VideoCodec은 한 세션에서 쓰는 압축 WebRTC 비디오 코덱이다. offer에서 골라
// answer를 만들기 전에 정해지고, 그 뒤로는 협상된 media section의 수명 동안
// 고정된다.
type VideoCodec string

const (
	VideoCodecVP8  VideoCodec = "video/VP8"
	VideoCodecH264 VideoCodec = "video/H264"
)

func (c VideoCodec) Valid() bool {
	return c == VideoCodecVP8 || c == VideoCodecH264
}

type commandFactory func(ctx context.Context, name string, arguments ...string) *exec.Cmd

// TranscoderOptions는 모든 세션의 FFmpeg 쌍이 공유하는 프로세스별 자원
// 정책을 담는다.
type TranscoderOptions struct {
	// Gate는 동시에 시작하는 FFmpeg 프로세스 수를 제한한다. nil이면 무제한이다.
	Gate *SpawnGate
	// EncoderThreads는 libvpx 인코더 스레드를 제한한다. 0이면 FFmpeg가 자동으로 정한다.
	EncoderThreads int
	// WireFormat은 AI 경계에서 주고받는 디코드 프레임 형식을 고른다.
	// JPEG(기본) 또는 raw yuv420p다.
	WireFormat config.WireFormat
	// VideoCodec은 협상된 WebRTC 코덱이다. SDP 코덱 선택에 참여하지 않는
	// 호출자를 위해 VP8이 zero-value 기본값으로 남아 있다.
	VideoCodec VideoCodec
	// PinLongEdge는 디코더 출력의 장변을 고정한다. 0(기본)이면 첫 프레임을
	// 따르는 종전 동작을 유지한다.
	PinLongEdge uint16
}

// decoderOutput은 디코더가 내보낼 치수와 그것을 강제하는 -vf 체인을 정한다.
// 핀이 꺼져 있으면(기본) 소스 치수를 그대로 쓰고 필터도 붙지 않으므로 종전과
// 인자가 바이트 동일하다.
func (t *FFmpegTranscoder) decoderOutput(sourceWidth, sourceHeight uint16) (uint16, uint16, string, error) {
	if t.options.PinLongEdge == 0 {
		return sourceWidth, sourceHeight, "", nil
	}
	width, height, err := pinDimensions(sourceWidth, sourceHeight, t.options.PinLongEdge)
	if err != nil {
		return 0, 0, "", err
	}
	return width, height, pinFilter(width, height), nil
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
	if !options.VideoCodec.Valid() {
		options.VideoCodec = VideoCodecVP8
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
	if t.options.VideoCodec == VideoCodecH264 {
		return t.decodeH264Stream(ctx, input, output)
	}
	return t.decodeVP8Stream(ctx, input, output)
}

func (t *FFmpegTranscoder) decodeVP8Stream(ctx context.Context, input <-chan frame, output chan<- frame) error {
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
	outputWidth, outputHeight, filter, err := t.decoderOutput(width, height)
	if err != nil {
		return err
	}
	process, err := t.startDecoder(ctx, width, height, filter)
	if err != nil {
		return err
	}
	defer process.close()
	// 디코더 ffmpeg는 출력 규격을 첫 프레임에 고정한다. 핀이 꺼져 있으면 이
	// 한 줄이 세션 화질의 상한을 기록하고, 켜져 있으면 입력과 무관하게
	// 유지되는 규격을 기록한다(#122).
	t.logger.Info("VP8 decoder output locked",
		"width", outputWidth, "height", outputHeight,
		"pin_long_edge", t.options.PinLongEdge, "wire_format", t.options.WireFormat)

	metadata := make(chan frame, 64)
	writeError := make(chan error, 1)
	go func() {
		defer close(metadata)
		defer process.stdin.Close()
		index := uint64(0)
		// observed*는 관측 전용이며 이 고루틴 안에서만 쓴다(레이스 없음).
		observedWidth, observedHeight := width, height
		write := func(item frame) error {
			// 키프레임에서만 치수를 읽을 수 있다. 인터프레임은 ErrKeyframeRequired로
			// 걸러진다. 메타데이터는 바꾸지 않고 관측만 남긴다(#122).
			if inputWidth, inputHeight, err := vp8KeyframeDimensions(item.data); err == nil &&
				(inputWidth != observedWidth || inputHeight != observedHeight) {
				t.logger.Info("VP8 input resolution changed",
					"from_width", observedWidth, "from_height", observedHeight,
					"to_width", inputWidth, "to_height", inputHeight,
					"decoder_locked_width", outputWidth, "decoder_locked_height", outputHeight)
				observedWidth, observedHeight = inputWidth, inputHeight
			}
			item.width, item.height = outputWidth, outputHeight
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

	rawSize := rawFrameSize(outputWidth, outputHeight)
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
	if t.options.VideoCodec == VideoCodecH264 {
		return t.encodeH264Stream(ctx, input, output)
	}
	return t.encodeVP8Stream(ctx, input, output)
}

func (t *FFmpegTranscoder) encodeVP8Stream(ctx context.Context, input <-chan frame, output chan<- frame) error {
	// 인코더 프로세스는 처리된 첫 프레임이 생긴 뒤에야 스폰한다. 디코더가
	// 키프레임을 기다리는 것과 같은 방식이다. 이렇게 해야 세션 N개가 한꺼번에
	// 뜨는 콜드 스파이크에서 미디어가 흐르기도 전에 인코더 N개를 fork하지 않는다.
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

// startDecoder의 width/height는 IVF 헤더에 쓰는 입력 기술자이므로 항상 소스
// 치수여야 한다. 출력 규격은 filter가 정한다(빈 문자열이면 입력을 따른다).
func (t *FFmpegTranscoder) startDecoder(ctx context.Context, width, height uint16, filter string) (*ffmpegProcess, error) {
	arguments := []string{
		"-hide_banner", "-loglevel", "error",
		"-probesize", "32", "-analyzeduration", "0", "-fpsprobesize", "0", "-threads", "1",
		"-f", "ivf", "-blocksize", "1024", "-i", "pipe:0",
	}
	if filter != "" {
		arguments = append(arguments, "-vf", filter)
	}
	if t.options.WireFormat == config.WireFormatRaw {
		arguments = append(arguments, "-an", "-f", "rawvideo", "-pix_fmt", "yuv420p", "-flush_packets", "1", "-blocksize", "1024")
	} else {
		arguments = append(arguments, "-an", "-strict", "unofficial", "-f", "image2pipe", "-vcodec", "mjpeg", "-q:v", "3", "-pix_fmt", "yuvj420p", "-flush_packets", "1", "-blocksize", "1024")
	}
	arguments = append(arguments, "pipe:1")
	process, err := t.startFFmpeg(ctx, "decoder", nil, nil, arguments...)
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
	process, err := t.startFFmpeg(ctx, "encoder", nil, nil, arguments...)
	if err != nil {
		return nil, fmt.Errorf("start FFmpeg VP8 encoder: %w", err)
	}
	return process, nil
}

// startFFmpeg는 FFmpeg 자식 하나를 스폰한다. stderrLine이 nil이 아니면 기본
// 경고 로그 대신 stderr의 각 줄을 그쪽으로 넘긴다(RTMP egress가 -progress
// 출력을 실제 에러와 분리하는 데 쓴다). extraFiles는 자식에게 fd 3번부터
// 전달되고(extraFiles[0] == fd 3), 자식이 시작된 뒤 부모 쪽 사본은 닫힌다.
// 그런 파이프의 쓰기 끝을 계속 들고 있어야 하는 호출자는 자기 사본을 따로 보관한다.
func (t *FFmpegTranscoder) startFFmpeg(ctx context.Context, role string, stderrLine func(string), extraFiles []*os.File, arguments ...string) (*ffmpegProcess, error) {
	logger := t.logger.With("ffmpeg_role", role)
	cmd := t.newCommand(ctx, t.path, arguments...)
	cmd.ExtraFiles = extraFiles
	// os.Pipe()는 논블로킹이면서 런타임 poller가 관리하는 디스크립터를 돌려준다.
	// FFmpeg는 pipe:N 입력을 평범한 블로킹 syscall로 읽고 EAGAIN을 파일 끝으로
	// 취급하므로, 데이터가 버퍼에 차기 전에 fd 3을 읽은 자식은 "Error opening
	// input: End of file"로 중단된다. 자식이 상속하기 전에 각 extra file을
	// poller에서 떼어내고 블로킹 모드로 되돌린다.
	for _, file := range extraFiles {
		_ = syscall.SetNonblock(int(file.Fd()), false)
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(os.Interrupt)
	}
	cmd.WaitDelay = ffmpegShutdownGrace
	// 파이프를 만들기 전에 gate를 먼저 얻는다. 큐에서 대기하다 취소된 시작이
	// 파일 디스크립터를 이미 할당해 두면 안 되기 때문이다(exec은 Start 자체의
	// 정리 경로로만 닫는데, 이 경로에서는 그게 실행되지 않는다).
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
	// 자식이 extra fd를 상속(dup)했으므로 부모 쪽 사본을 닫는다. 그래야 쓰는
	// 쪽이 닫힐 때 오디오 파이프의 읽기 끝이 EOF에 도달한다.
	for _, file := range extraFiles {
		_ = file.Close()
	}
	if err != nil {
		return nil, err
	}
	if t.metrics != nil {
		t.metrics.RegisterChildProcess(cmd.Process.Pid)
	}
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if stderrLine != nil {
				stderrLine(scanner.Text())
				continue
			}
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

// rawFrameSize는 yuv420p 프레임 한 장의 바이트 크기를 돌려준다. 홀수 치수는
// FFmpeg가 내보내는 방식 그대로 크로마 평면을 올림 처리한다.
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

// readJPEG는 버퍼링한 구간에서 EOI 마커를 훑어 바이트 스트림에서 JPEG 이미지
// 하나를 꺼낸다. 0xFFD9 시퀀스는 엔트로피 코딩 데이터 안에 나타날 수 없으므로
// (바이트 스터핑이 0xFF를 0xFF00으로 이스케이프한다) 처음 나온 것이 이미지의
// 끝이다.
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
