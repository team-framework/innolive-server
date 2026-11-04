package media

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"inno-live-server/internal/metrics"
)

// TestSegmentsRestartOnOrientationChange는 가로 영상이 세로로 바뀔 때 디코더가 그
// 키프레임부터 새 구간을 띄우고, 인코더도 크기가 바뀐 프레임부터 새 구간을 띄워
// 프레임을 잃지 않는지 실제 ffmpeg로 본다(#449, P2-1).
func TestSegmentsRestartOnOrientationChange(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	const perClip = 6
	for _, codec := range []VideoCodec{VideoCodecVP8, VideoCodecH264} {
		t.Run(string(codec), func(t *testing.T) {
			var units [][]byte
			for _, size := range []string{"320x180", "180x320"} {
				units = append(units, segmentTestClip(t, ffmpegPath, codec, size, perClip)...)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			transcoder := NewFFmpegTranscoder(ffmpegPath, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), TranscoderOptions{VideoCodec: codec})
			input := make(chan frame, len(units))
			decoded := make(chan frame, len(units))
			tapped := make(chan frame, len(units))
			encoded := make(chan frame, len(units))
			decodeError := make(chan error, 1)
			encodeError := make(chan error, 1)
			var sizes [][2]uint16
			go func() {
				decodeError <- transcoder.DecodeStream(ctx, input, decoded, nil)
				close(decoded)
			}()
			go func() {
				defer close(tapped)
				for item := range decoded {
					sizes = append(sizes, [2]uint16{item.width, item.height})
					tapped <- item
				}
			}()
			go func() {
				encodeError <- transcoder.EncodeStream(ctx, tapped, encoded)
				close(encoded)
			}()
			for index, data := range units {
				input <- frame{data: data, timestamp: uint32(index * 3000)}
			}
			close(input)
			count := 0
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
			if count != len(units) {
				t.Fatalf("encoded frames = %d, want %d", count, len(units))
			}
			for index, size := range sizes {
				want := [2]uint16{320, 180}
				if index >= perClip {
					want = [2]uint16{180, 320}
				}
				if size != want {
					t.Fatalf("decoded frame %d size = %v, want %v", index, size, want)
				}
			}
		})
	}
}

// segmentTestClip은 첫 프레임이 키프레임인 짧은 영상을 프레임 단위로 만든다.
func segmentTestClip(t *testing.T, ffmpegPath string, codec VideoCodec, size string, frames int) [][]byte {
	t.Helper()
	source := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=" + size + ":rate=30", "-frames:v", strconv.Itoa(frames), "-pix_fmt", "yuv420p"}
	if codec == VideoCodecH264 {
		stream, err := exec.Command(ffmpegPath, append(source,
			"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-profile:v", "baseline",
			"-g", "30", "-bf", "0", "-x264-params", "repeat-headers=1:aud=1:annexb=1", "-f", "h264", "pipe:1")...).Output()
		if err != nil {
			t.Fatalf("generate H.264 %s: %v", size, err)
		}
		reader := h264AccessUnitReader{reader: bufio.NewReader(bytes.NewReader(stream))}
		var units [][]byte
		for {
			unit, err := reader.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			units = append(units, unit)
		}
		if len(units) != frames {
			t.Fatalf("H.264 %s access units = %d, want %d", size, len(units), frames)
		}
		return units
	}
	path := filepath.Join(t.TempDir(), "clip.ivf")
	if output, err := exec.Command(ffmpegPath, append(source,
		"-c:v", "libvpx", "-deadline", "realtime", "-g", "30", path)...).CombinedOutput(); err != nil {
		t.Fatalf("generate VP8 %s: %v: %s", size, err, output)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := io.ReadFull(file, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	units := make([][]byte, 0, frames)
	for range frames {
		data, _, err := readIVFFrame(file)
		if err != nil {
			t.Fatal(err)
		}
		units = append(units, data)
	}
	return units
}
