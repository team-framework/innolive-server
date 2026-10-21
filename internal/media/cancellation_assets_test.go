package media

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os/exec"
	"testing"

	"inno-live-server/internal/config"
)

func TestCancellationSlateCropRectUsesCenteredAspectPreservingCrop(t *testing.T) {
	landscape := image.Rect(0, 0, 1920, 1080)
	portrait := image.Rect(0, 0, 1080, 1920)
	cases := []struct {
		name         string
		source       image.Rectangle
		targetWidth  uint16
		targetHeight uint16
		want         image.Rectangle
	}{
		{
			name:         "same aspect keeps landscape asset intact",
			source:       landscape,
			targetWidth:  1280,
			targetHeight: 720,
			want:         landscape,
		},
		{
			name:         "15 by 9 crops landscape sides symmetrically",
			source:       landscape,
			targetWidth:  1500,
			targetHeight: 900,
			want:         image.Rect(60, 0, 1860, 1080),
		},
		{
			name:         "square crops landscape sides around centered message",
			source:       landscape,
			targetWidth:  1080,
			targetHeight: 1080,
			want:         image.Rect(420, 0, 1500, 1080),
		},
		{
			name:         "3 by 4 crops portrait top and bottom symmetrically",
			source:       portrait,
			targetWidth:  900,
			targetHeight: 1200,
			want:         image.Rect(0, 240, 1080, 1680),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cancellationSlateCropRect(tc.source, tc.targetWidth, tc.targetHeight); got != tc.want {
				t.Fatalf("cancellationSlateCropRect(%v, %dx%d) = %v, want %v", tc.source, tc.targetWidth, tc.targetHeight, got, tc.want)
			}
		})
	}
}

func TestRenderCancellationSlateUsesCenteredCrop(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 8, 4))
	columns := []color.RGBA{
		{R: 255, A: 255},
		{R: 192, A: 255},
		{G: 255, A: 255},
		{G: 192, A: 255},
		{B: 192, A: 255},
		{B: 255, A: 255},
		{R: 192, G: 192, A: 255},
		{R: 255, G: 255, A: 255},
	}
	for y := 0; y < source.Bounds().Dy(); y++ {
		for x, pixel := range columns {
			source.SetRGBA(x, y, pixel)
		}
	}

	// 2:1 source를 1:1로 만들면 중앙 네 열(2~5)만 남는다. stretch로
	// 되돌리면 첫 번째와 마지막 픽셀이 각각 columns[2], columns[5]가 아니므로
	// 이 검증은 실제 렌더링 경로가 crop 영역을 사용하는지 잡아낸다.
	rendered := renderCancellationSlate(source, 4, 4)
	for x, want := range columns[2:6] {
		if got := rendered.RGBAAt(x, 2); got != want {
			t.Fatalf("rendered pixel at x=%d = %#v, want centered-crop pixel %#v", x, got, want)
		}
	}
}

// TestZoomCropRectMatchesFFmpegCrop은 ffmpeg 8.1.1에서 실측한 crop 영역을 고정한다.
// 시작 좌표가 홀수가 되는 경우(720 → 33) ffmpeg처럼 짝수로 내려야 한다.
func TestZoomCropRectMatchesFFmpegCrop(t *testing.T) {
	cases := []struct {
		width, height uint16
		want          image.Rectangle
	}{
		{720, 1280, image.Rect(32, 58, 686, 1220)},
		{1280, 720, image.Rect(58, 32, 1220, 686)},
		{1080, 1920, image.Rect(50, 88, 1030, 1832)},
		{270, 480, image.Rect(12, 22, 256, 458)},
	}
	for _, tc := range cases {
		if got := zoomCropRect(tc.width, tc.height); got != tc.want {
			t.Errorf("zoomCropRect(%d, %d) = %v, want %v", tc.width, tc.height, got, tc.want)
		}
	}
}

// TestCancellationSlateUnzoomedAfterEgressFilter는 슬레이트가 실제 송출 필터의
// 1.1배 확대를 거친 뒤 1배 슬레이트와 같아지는지 본다. 보정하지 않은 슬레이트는
// 같은 필터를 거치면 확연히 달라야 이 검사가 의미를 가진다.
func TestCancellationSlateUnzoomedAfterEgressFilter(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	for _, size := range [][2]uint16{{720, 1280}, {1280, 720}} {
		width, height := size[0], size[1]
		t.Run(fmt.Sprintf("%dx%d", width, height), func(t *testing.T) {
			source, err := png.Decode(bytes.NewReader(cancellationSlateAsset(width, height)))
			if err != nil {
				t.Fatalf("decode slate asset: %v", err)
			}
			want := rgbaToYUV420P(renderCancellationSlate(source, width, height))
			slate, err := cancellationSlateFrame(width, height, config.WireFormatRaw)
			if err != nil {
				t.Fatalf("cancellationSlateFrame: %v", err)
			}
			filter := newTestEgress(config.WireFormatRaw, "out.flv").videoFilter(width, height)

			compensated := lumaMeanAbsDiff(runRawFilter(t, slate.data, width, height, filter), want, width, height)
			uncompensated := lumaMeanAbsDiff(runRawFilter(t, want, width, height, filter), want, width, height)
			t.Logf("luma 평균 차이: 보정 %.3f, 미보정 %.3f", compensated, uncompensated)
			if compensated > 0.5 {
				t.Fatalf("확대 뒤 슬레이트가 1배와 다르다: 평균 차이 %.3f", compensated)
			}
			if uncompensated < compensated*4 {
				t.Fatalf("미보정 슬레이트와 구별되지 않는다: 보정 %.3f, 미보정 %.3f", compensated, uncompensated)
			}
		})
	}
}

func runRawFilter(t *testing.T, input []byte, width, height uint16, filter string) []byte {
	t.Helper()
	size := fmt.Sprintf("%dx%d", width, height)
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "rawvideo", "-pixel_format", "yuv420p", "-video_size", size, "-i", "pipe:0",
		"-vf", filter, "-f", "rawvideo", "-pix_fmt", "yuv420p", "pipe:1")
	command.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("ffmpeg %s: %v\n%s", filter, err, stderr.String())
	}
	if len(output) != rawFrameSize(width, height) {
		t.Fatalf("ffmpeg output is %d bytes, want %d", len(output), rawFrameSize(width, height))
	}
	return output
}

func lumaMeanAbsDiff(got, want []byte, width, height uint16) float64 {
	pixels := int(width) * int(height)
	total := 0
	for i := 0; i < pixels; i++ {
		difference := int(got[i]) - int(want[i])
		if difference < 0 {
			difference = -difference
		}
		total += difference
	}
	return float64(total) / float64(pixels)
}
