package media

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

// captureEgressArguments는 한 번의 프로세스 시작에서 FFmpeg에 넘어간 인자를
// 그대로 돌려준다. 실제 FFmpeg는 실행하지 않는다.
func captureEgressArguments(t *testing.T, options TranscoderOptions) []string {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := NewRTMPEgress("ffmpeg", logger, metrics.New(), options, "rtmp://host/live2/secret-key", nil, false, 0, "", "")
	var captured []string
	e.transcoder.newCommand = func(ctx context.Context, _ string, arguments ...string) *exec.Cmd {
		captured = arguments
		return exec.CommandContext(ctx, "__innolive_missing_ffmpeg_for_encoder_test__")
	}
	// 시작은 실패해도 된다 — 인자는 그 전에 조립된다.
	_, _ = e.start(context.Background(), 1280, 720, 30)
	if captured == nil {
		t.Fatal("no FFmpeg invocation was captured")
	}
	return captured
}

// segment는 인자 벡터에서 연속된 부분 수열의 시작 위치를 찾는다.
func containsSegment(arguments, want []string) bool {
	for index := 0; index+len(want) <= len(arguments); index++ {
		if slices.Equal(arguments[index:index+len(want)], want) {
			return true
		}
	}
	return false
}

// TestEgressDefaultEncoderStaysX264: 기본값은 종전 libx264 인자 그대로다.
// 이 인자가 바뀌면 유튜브 송출 화질·지연이 조용히 달라진다.
func TestEgressDefaultEncoderStaysX264(t *testing.T) {
	arguments := captureEgressArguments(t, TranscoderOptions{WireFormat: config.WireFormatJPEG})

	want := []string{
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency",
		"-pix_fmt", "yuv420p", "-profile:v", "main",
		"-b:v", "2500k", "-maxrate", "2500k", "-bufsize", "2500k",
		"-g", "60", "-bf", "0",
		"-r", "30", "-fps_mode", "cfr",
	}
	if !containsSegment(arguments, want) {
		t.Fatalf("default arguments = %v\nwant the x264 segment %v", arguments, want)
	}
	if slices.Contains(arguments, "h264_nvenc") || slices.Contains(arguments, "-gpu") {
		t.Fatalf("default must not use NVENC: %v", arguments)
	}
}

// TestEgressNVENCArguments: NVENC로 켜면 x264 인자가 사라지고 프로덕션
// 컨테이너(ffmpeg 5.1.9)에서 확인한 조합이 나간다.
func TestEgressNVENCArguments(t *testing.T) {
	arguments := captureEgressArguments(t, TranscoderOptions{
		WireFormat:         config.WireFormatJPEG,
		EgressVideoEncoder: config.EgressVideoEncoderNVENC,
	})

	want := []string{"-c:v", "h264_nvenc", "-preset", "p1", "-tune", "ll", "-rc", "cbr"}
	if !containsSegment(arguments, want) {
		t.Fatalf("NVENC arguments = %v\nwant the segment %v", arguments, want)
	}
	if slices.Contains(arguments, "libx264") {
		t.Fatalf("NVENC must replace libx264: %v", arguments)
	}
	// 비트레이트 3종은 인코더와 무관하게 같이 간다 — cbr이 상한을 지키는 근거다.
	if !containsSegment(arguments, []string{"-b:v", "2500k", "-maxrate", "2500k", "-bufsize", "2500k"}) {
		t.Fatalf("NVENC arguments must keep the bitrate trio: %v", arguments)
	}
	// GPU 개수를 주지 않았으면 -gpu는 붙지 않는다(FFmpeg에 맡긴다).
	if slices.Contains(arguments, "-gpu") {
		t.Fatalf("-gpu must be omitted when no GPU count is configured: %v", arguments)
	}
}

// TestEgressNVENCDistributesAcrossGPUs: FFmpeg는 카드 사이에 자동 분산하지
// 않는다. 지정하지 않으면 전부 GPU 0에 몰려 카드당 한계(12세션)에 먼저 닿는다.
func TestEgressNVENCDistributesAcrossGPUs(t *testing.T) {
	options := TranscoderOptions{
		WireFormat:         config.WireFormatJPEG,
		EgressVideoEncoder: config.EgressVideoEncoderNVENC,
		NVENCGPUs:          2,
	}
	first := gpuArgument(t, captureEgressArguments(t, options))
	second := gpuArgument(t, captureEgressArguments(t, options))
	if first == second {
		t.Fatalf("two consecutive starts both used GPU %s", first)
	}
	for _, device := range []string{first, second} {
		if device != "0" && device != "1" {
			t.Fatalf("-gpu = %s, want 0 or 1", device)
		}
	}
}

func gpuArgument(t *testing.T, arguments []string) string {
	t.Helper()
	index := slices.Index(arguments, "-gpu")
	if index < 0 || index+1 >= len(arguments) {
		t.Fatalf("no -gpu argument in %v", arguments)
	}
	// -gpu는 인코더 사설 옵션이라 -c:v 뒤에 와야 한다.
	if codec := slices.Index(arguments, "-c:v"); codec > index {
		t.Fatalf("-gpu must follow -c:v: %s", strings.Join(arguments, " "))
	}
	return arguments[index+1]
}

// TestNextNVENCDevice: 카드가 하나거나 미지정이면 항상 0이다.
func TestNextNVENCDevice(t *testing.T) {
	for _, count := range []int{-1, 0, 1} {
		if device := nextNVENCDevice(count); device != 0 {
			t.Fatalf("nextNVENCDevice(%d) = %d, want 0", count, device)
		}
	}
	// 카드가 둘이면 연속 호출이 번갈아 나온다.
	first, second, third := nextNVENCDevice(2), nextNVENCDevice(2), nextNVENCDevice(2)
	if first == second || first != third {
		t.Fatalf("round robin over 2 GPUs = %d, %d, %d", first, second, third)
	}
}
