package media

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"inno-live-server/internal/metrics"
)

func TestVP8KeyframeDimensions(t *testing.T) {
	frame := []byte{0x00, 0x00, 0x00, 0x9d, 0x01, 0x2a, 0x00, 0x05, 0xd0, 0x02}
	width, height, err := vp8KeyframeDimensions(frame)
	if err != nil {
		t.Fatal(err)
	}
	if width != 1280 || height != 720 {
		t.Fatalf("dimensions = %dx%d", width, height)
	}
}

func TestVP8InterframeRequiresKeyframe(t *testing.T) {
	_, _, err := vp8KeyframeDimensions([]byte{0x01, 0, 0})
	if !errors.Is(err, ErrKeyframeRequired) {
		t.Fatalf("error = %v", err)
	}
}

func TestIVFFrameRoundTrip(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(ivfHeader(1280, 720, 30, 1))
	input := []byte{1, 2, 3, 4}
	if err := writeIVFFrame(&stream, input, 9); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 32)
	if _, err := stream.Read(header); err != nil {
		t.Fatal(err)
	}
	if string(header[:4]) != "DKIF" || binary.LittleEndian.Uint16(header[12:14]) != 1280 {
		t.Fatalf("invalid IVF header: %x", header)
	}
	output, timestamp, err := readIVFFrame(&stream)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, input) || timestamp != 9 {
		t.Fatalf("frame=%v timestamp=%d", output, timestamp)
	}
}

func TestReadJPEGStopsAtFrameBoundary(t *testing.T) {
	reader := bufio.NewReader(bytes.NewReader([]byte{0xff, 0xd8, 1, 2, 0xff, 0xd9, 0xff, 0xd8, 3, 0xff, 0xd9}))
	first, err := readJPEG(reader)
	if err != nil {
		t.Fatal(err)
	}
	second, err := readJPEG(reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 6 || len(second) != 5 {
		t.Fatalf("JPEG lengths = %d, %d", len(first), len(second))
	}
}

func TestFFmpegTranscoderHandlesVP8Interframes(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	inputPath := filepath.Join(t.TempDir(), "input.ivf")
	generate := exec.Command(
		ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=1280x720:rate=30",
		"-frames:v", "10", "-c:v", "libvpx", "-deadline", "realtime", "-g", "30",
		inputPath,
	)
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate VP8 fixture: %v: %s", err, output)
	}
	file, err := os.Open(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	header := make([]byte, 32)
	if _, err := io.ReadFull(file, header); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	transcoder := NewFFmpegTranscoder(ffmpegPath, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), TranscoderOptions{})
	input := make(chan frame, 10)
	decoderOutput := make(chan frame, 10)
	decoded := make(chan frame, 10)
	encoded := make(chan frame, 10)
	firstDecoded := make(chan struct{}, 1)
	decodeError := make(chan error, 1)
	encodeError := make(chan error, 1)
	go func() {
		decodeError <- transcoder.DecodeStream(ctx, input, decoderOutput)
		close(decoderOutput)
	}()
	go func() {
		defer close(decoded)
		for item := range decoderOutput {
			select {
			case firstDecoded <- struct{}{}:
			default:
			}
			decoded <- item
		}
	}()
	go func() {
		encodeError <- transcoder.EncodeStream(ctx, decoded, encoded)
		close(encoded)
	}()
	for index := 0; index < 10; index++ {
		data, _, err := readIVFFrame(file)
		if err != nil {
			t.Fatalf("read input frame %d: %v", index, err)
		}
		input <- frame{data: data, timestamp: uint32(index * 3000)}
	}
	count := 0
	select {
	case output := <-encoded:
		if len(output.data) == 0 {
			t.Fatal("first encoded frame is empty")
		}
		count++
	case <-time.After(2 * time.Second):
		select {
		case <-firstDecoded:
			t.Fatal("FFmpeg encoder did not produce a frame before input EOF")
		default:
			t.Fatal("FFmpeg decoder did not produce a frame before input EOF")
		}
	}
	close(input)
	for output := range encoded {
		if len(output.data) == 0 {
			t.Fatalf("encoded frame %d is empty", count)
		}
		count++
	}
	if err := <-decodeError; err != nil {
		t.Fatalf("DecodeStream() error = %v", err)
	}
	if err := <-encodeError; err != nil {
		t.Fatalf("EncodeStream() error = %v", err)
	}
	if count != 10 {
		t.Fatalf("encoded frame count = %d, want 10", count)
	}
}

func TestFFmpegTranscoderHandlesH264AccessUnits(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	stream, err := exec.Command(
		ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x180:rate=30",
		"-frames:v", "10", "-pix_fmt", "yuv420p", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-profile:v", "baseline", "-g", "1", "-bf", "0", "-x264-params", "repeat-headers=1:aud=1:annexb=1",
		"-f", "h264", "pipe:1",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("generate H.264 fixture: %v: %s", err, stream)
	}
	reader := h264AccessUnitReader{reader: bufio.NewReader(bytes.NewReader(stream))}
	frames := make([][]byte, 0, 10)
	for {
		frame, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	if len(frames) != 10 {
		t.Fatalf("H.264 access units = %d, want 10", len(frames))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	transcoder := NewFFmpegTranscoder(ffmpegPath, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), TranscoderOptions{VideoCodec: VideoCodecH264})
	input := make(chan frame, len(frames))
	decoderOutput := make(chan frame, len(frames))
	encoded := make(chan frame, len(frames))
	decodeError := make(chan error, 1)
	encodeError := make(chan error, 1)
	go func() {
		decodeError <- transcoder.DecodeStream(ctx, input, decoderOutput)
		close(decoderOutput)
	}()
	go func() {
		encodeError <- transcoder.EncodeStream(ctx, decoderOutput, encoded)
		close(encoded)
	}()
	for index, data := range frames {
		input <- frame{data: data, timestamp: uint32(index * 3000)}
	}
	close(input)
	count := 0
	for output := range encoded {
		if len(output.data) == 0 || len(splitAnnexBNALUs(output.data)) == 0 {
			t.Fatalf("encoded H.264 frame %d is invalid", count)
		}
		count++
	}
	if err := <-decodeError; err != nil {
		t.Fatalf("DecodeStream() error = %v", err)
	}
	if err := <-encodeError; err != nil {
		t.Fatalf("EncodeStream() error = %v", err)
	}
	if count != len(frames) {
		t.Fatalf("encoded H.264 frames = %d, want %d", count, len(frames))
	}
}
