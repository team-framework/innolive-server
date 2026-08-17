package media

import (
	"image"
	"image/color"
	"testing"
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
