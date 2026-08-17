package media

import (
	"image"
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
