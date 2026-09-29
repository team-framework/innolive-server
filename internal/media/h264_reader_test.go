package media

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os/exec"
	"testing"
	"time"
)

type oneByteH264Reader struct{ data []byte }

func (r *oneByteH264Reader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func TestH264AccessUnitReaderAcrossChunkBoundaries(t *testing.T) {
	first := []byte{0, 0, 1, 7, 0x42, 0, 0, 0, 1, 9, 0xf0, 0, 0, 1, 5, 0xaa}
	second := []byte{0, 0, 1, 9, 0xf0, 0, 0, 0, 1, 1, 0xbb}
	third := []byte{0, 0, 0, 1, 9, 0xf0, 0, 0, 1, 1, 0xcc}
	stream := append(append(append([]byte(nil), first...), second...), third...)
	reader := h264AccessUnitReader{reader: bufio.NewReader(&oneByteH264Reader{data: stream})}
	for index, want := range [][]byte{first, second, third} {
		got, err := reader.Read()
		if err != nil {
			t.Fatalf("Read() unit %d: %v", index, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Read() unit %d = %x, want %x", index, got, want)
		}
	}
	if _, err := reader.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("Read() after final unit = %v, want EOF", err)
	}
}

func TestH264AccessUnitReaderKeepsFinalDataWithoutAUD(t *testing.T) {
	data := []byte{0, 0, 1, 7, 0x42, 0, 0, 1, 8, 0x99}
	reader := h264AccessUnitReader{reader: bufio.NewReader(bytes.NewReader(data))}
	got, err := reader.Read()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("final data without AUD = %x, %v; want %x", got, err, data)
	}
	if _, err := reader.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("Read() after final data = %v, want EOF", err)
	}
}

func TestH264AccessUnitReaderDoesNotRescanLargeUnit(t *testing.T) {
	first := append([]byte{0, 0, 0, 1, 9, 0xf0, 0, 0, 1, 5}, bytes.Repeat([]byte{0x55}, 128<<10)...)
	stream := append(append([]byte(nil), first...), 0, 0, 0, 1, 9, 0xf0)
	reader := h264AccessUnitReader{reader: bufio.NewReader(bytes.NewReader(stream))}
	start := time.Now()
	got, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, first) {
		t.Fatalf("first H.264 access unit differs: got %d bytes, want %d", len(got), len(first))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("splitting a 128 KiB H.264 unit took %s; previously scanned the entire buffer for every byte", elapsed)
	}
}

func TestH264AccessUnitReaderRejectsOversizedUnit(t *testing.T) {
	stream := append([]byte{0, 0, 0, 1, 9, 0xf0}, bytes.Repeat([]byte{0x55}, maxEncodedFrameSize)...)
	reader := h264AccessUnitReader{reader: bufio.NewReader(bytes.NewReader(stream))}
	if _, err := reader.Read(); err == nil || err.Error() != "H.264 access unit exceeds maximum size" {
		t.Fatalf("oversized unit error = %v", err)
	}
}

func TestH264AccessUnitReaderAllowsMaximumSize(t *testing.T) {
	first := append([]byte{0, 0, 0, 1, 9, 0xf0}, bytes.Repeat([]byte{0x55}, maxEncodedFrameSize-6)...)
	stream := append(append([]byte(nil), first...), 0, 0, 0, 1, 9, 0xf0)
	reader := h264AccessUnitReader{reader: bufio.NewReader(bytes.NewReader(stream))}
	got, err := reader.Read()
	if err != nil || !bytes.Equal(got, first) {
		t.Fatalf("maximum-sized unit: got %d bytes, error %v; want %d bytes", len(got), err, len(first))
	}
}

func BenchmarkH264AccessUnitReaderRealStream(b *testing.B) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		b.Skip("ffmpeg is not installed")
	}
	stream, err := exec.Command(ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=720x1280:rate=30", "-frames:v", "30",
		"-threads", "1", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-profile:v", "baseline", "-level:v", "3.1", "-pix_fmt", "yuv420p",
		"-g", "30", "-keyint_min", "30", "-sc_threshold", "0", "-bf", "0",
		"-x264-params", "repeat-headers=1:aud=1:annexb=1", "-f", "h264", "pipe:1",
	).CombinedOutput()
	if err != nil {
		b.Fatalf("generate H.264 stream: %v: %s", err, stream)
	}
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		reader := h264AccessUnitReader{reader: bufio.NewReader(bytes.NewReader(stream))}
		count := 0
		for {
			_, err := reader.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			count++
		}
		if count != 30 {
			b.Fatalf("read %d access units, want 30", count)
		}
	}
}
