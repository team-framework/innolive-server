//go:build egress_harness

// 디코더 해상도 관측 로그 검증(#124). 실 ffmpeg가 필요하므로 egress_harness
// 빌드 태그 뒤에 두고, 도구가 없으면 건너뛴다.
//
//	go test -tags egress_harness -run TestDecoderLogs ./internal/media -v
package media

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

// splitAccessUnits는 Annex-B 스트림을 액세스 유닛 단위로 쪼갠다.
func splitAccessUnits(t *testing.T, stream []byte) [][]byte {
	t.Helper()
	reader := &h264AccessUnitReader{reader: bufio.NewReader(bytes.NewReader(stream))}
	var units [][]byte
	for {
		unit, err := reader.Read()
		if err != nil {
			return units
		}
		units = append(units, unit)
	}
}

// drainDecoder는 디코드 결과를 모두 비우고 프레임 수를 센다. 출력을 읽지
// 않으면 디코더가 파이프에서 막힌다.
func drainDecoder(t *testing.T, transcoder *FFmpegTranscoder, frames [][]byte) (string, int) {
	t.Helper()
	logs := &syncLogBuffer{}
	transcoder.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	input := make(chan frame, 512)
	output := make(chan frame, 512)
	timestamp := uint32(90000)
	for _, data := range frames {
		input <- frame{data: data, timestamp: timestamp}
		timestamp += 3000
	}
	close(input)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	go func() { _ = transcoder.DecodeStream(ctx, input, output); close(output) }()

	decoded := 0
	for range output {
		decoded++
	}
	return logs.String(), decoded
}

// TestDecoderLogsH264ResolutionChange는 H.264 입력 해상도가 스트림 도중
// 올라갔을 때 (a) 디코더가 어디에 고정됐는지 (b) 입력이 어디까지 올라왔는지가
// 로그에 남는지 확인한다. 프로덕션에서 이 두 값의 격차가 곧 화질 손실이다.
func TestDecoderLogsH264ResolutionChange(t *testing.T) {
	requireTool(t, "ffmpeg")

	dir := t.TempDir()
	stream := append(
		annexBStream(t, dir, "ramp.h264", 640, 360),
		annexBStream(t, dir, "full.h264", 1280, 720)...,
	)
	units := splitAccessUnits(t, stream)
	if len(units) == 0 {
		t.Fatal("액세스 유닛 0개")
	}

	transcoder := NewFFmpegTranscoder("ffmpeg", slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(),
		TranscoderOptions{VideoCodec: VideoCodecH264, WireFormat: config.WireFormatJPEG})
	output, decoded := drainDecoder(t, transcoder, units)

	if decoded == 0 {
		t.Fatal("디코드된 프레임이 없다")
	}
	assertLogged(t, output, "H.264 decoder output locked", "width=640", "height=360")
	assertLogged(t, output, "H.264 input resolution changed", "to_width=1280", "to_height=720")
	t.Logf("액세스 유닛 %d개 → 디코드 %d프레임", len(units), decoded)
}

// TestDecoderLogsVP8ResolutionChange는 VP8 경로에도 같은 관측이 남는지 본다.
// 프로덕션 브라우저 세션은 실측 VP8로 협상되므로 이쪽이 실사용 경로다.
func TestDecoderLogsVP8ResolutionChange(t *testing.T) {
	requireTool(t, "ffmpeg")

	dir := t.TempDir()
	frames := append(
		vp8Frames(t, dir, "ramp.ivf", 640, 360),
		vp8Frames(t, dir, "full.ivf", 1280, 720)...,
	)
	if len(frames) == 0 {
		t.Fatal("VP8 프레임 0개")
	}

	transcoder := NewFFmpegTranscoder("ffmpeg", slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(),
		TranscoderOptions{VideoCodec: VideoCodecVP8, WireFormat: config.WireFormatJPEG})
	output, decoded := drainDecoder(t, transcoder, frames)

	if decoded == 0 {
		t.Fatal("디코드된 프레임이 없다")
	}
	assertLogged(t, output, "VP8 decoder output locked", "width=640", "height=360")
	assertLogged(t, output, "VP8 input resolution changed", "to_width=1280", "to_height=720")
	t.Logf("VP8 프레임 %d개 → 디코드 %d프레임", len(frames), decoded)
}

func assertLogged(t *testing.T, output string, message string, fields ...string) {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, message) {
			continue
		}
		missing := make([]string, 0, len(fields))
		for _, field := range fields {
			if !strings.Contains(line, field) {
				missing = append(missing, field)
			}
		}
		if len(missing) == 0 {
			return
		}
		t.Errorf("%q 로그에 %v가 없다:\n  %s", message, missing, line)
		return
	}
	t.Errorf("%q 로그가 남지 않았다", message)
}

// vp8Frames는 지정 해상도의 VP8 IVF를 만들고 프레임 페이로드만 뽑는다.
func vp8Frames(t *testing.T, dir, name string, width, height int) [][]byte {
	t.Helper()
	path := dir + "/" + name
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=%dx%d:rate=30:duration=1", width, height),
		"-c:v", "libvpx", "-b:v", "1M", "-pix_fmt", "yuv420p", "-g", "15",
		"-f", "ivf", path)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate %s: %v\n%s", name, err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	// IVF: 파일 헤더 32바이트, 프레임마다 12바이트 헤더(size uint32 + pts uint64).
	if len(data) < 32 {
		t.Fatalf("IVF가 너무 짧다: %d바이트", len(data))
	}
	var frames [][]byte
	for offset := 32; offset+12 <= len(data); {
		size := int(binary.LittleEndian.Uint32(data[offset : offset+4]))
		offset += 12
		if size <= 0 || offset+size > len(data) {
			break
		}
		frames = append(frames, append([]byte(nil), data[offset:offset+size]...))
		offset += size
	}
	return frames
}
