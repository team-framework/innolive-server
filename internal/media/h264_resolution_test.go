//go:build egress_harness

// H.264 디코드 경로의 해상도 메타데이터 추적 테스트. 실 ffmpeg가 필요하므로
// egress_harness 빌드 태그 뒤에 둔다.
//
//	go test -tags egress_harness -run TestH264Decode ./internal/media -v
package media

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

// TestH264DecodeTracksSPSResolutionChange는 #122의 수용 기준이다.
//
// 스트림 도중 SPS 해상도가 640x360에서 1280x720으로 바뀌어도 decodeH264Stream이
// 내보내는 frame.width/height는 최초 IDR의 값에 고정된다 — h264.go에서 후속
// 액세스 유닛의 치수를 `data, _, _, _ :=`로 버리기 때문이다. 이 때문에 egress의
// resolutionChanged가 울리지 않아 송출 해상도가 시작 시점에 묶였고(#121),
// 뷰어 인코더도 같은 값에 고정된다.
//
// #121은 EGRESS_VIDEO_SIZE로 송출 쪽 증상만 덮었다. 메타데이터를 실제로
// 갱신하려면 램프업 중 재기동이 반복되지 않도록 디바운스와 뷰어 인코더 재기동
// 경로가 함께 필요하므로 #122로 분리했다. 그 작업이 끝나면 이 Skip을 지운다.
func TestH264DecodeTracksSPSResolutionChange(t *testing.T) {
	t.Skip("#122: H.264 SPS 해상도 변경이 frame 메타데이터에 반영되지 않는다. 근본 수정 시 이 Skip을 제거한다.")

	requireTool(t, "ffmpeg")

	dir := t.TempDir()
	stream := append(
		annexBStream(t, dir, "ramp.h264", 640, 360),
		annexBStream(t, dir, "full.h264", 1280, 720)...,
	)

	transcoder := NewFFmpegTranscoder("ffmpeg", slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(),
		TranscoderOptions{VideoCodec: VideoCodecH264, WireFormat: config.WireFormatJPEG})

	input := make(chan frame, 256)
	output := make(chan frame, 256)
	reader := &h264AccessUnitReader{reader: bufio.NewReader(bytes.NewReader(stream))}
	units := 0
	for {
		unit, err := reader.Read()
		if err != nil {
			break
		}
		input <- frame{data: unit, timestamp: uint32(90000 + units*3000)}
		units++
	}
	close(input)
	if units == 0 {
		t.Fatal("액세스 유닛을 하나도 만들지 못했다")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	go func() { _ = transcoder.DecodeStream(ctx, input, output); close(output) }()

	sizes := map[string]int{}
	for item := range output {
		sizes[fmt.Sprintf("%dx%d", item.width, item.height)]++
	}
	t.Logf("액세스 유닛 %d개 공급, 디코더가 내보낸 메타데이터 분포: %v", units, sizes)
	if sizes["1280x720"] == 0 {
		t.Errorf("SPS가 1280x720으로 바뀐 뒤에도 frame.width/height가 갱신되지 않았다 (분포=%v)", sizes)
	}
}

// annexBStream은 지정 해상도의 짧은 Annex-B H.264 스트림을 만든다. AUD를 넣어
// h264AccessUnitReader가 액세스 유닛 경계를 찾을 수 있게 한다.
func annexBStream(t *testing.T, dir, name string, width, height int) []byte {
	t.Helper()
	path := dir + "/" + name
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=%dx%d:rate=30:duration=1", width, height),
		"-c:v", "libx264", "-preset", "ultrafast", "-profile:v", "baseline", "-pix_fmt", "yuv420p",
		"-g", "15", "-x264-params", "aud=1:repeat-headers=1:annexb=1",
		"-f", "h264", path)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate %s: %v\n%s", name, err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}
