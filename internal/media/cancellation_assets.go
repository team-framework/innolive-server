package media

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	"inno-live-server/internal/config"

	"golang.org/x/image/draw"
)

//go:embed assets/cancel-landscape.png
var cancelLandscapePNG []byte

//go:embed assets/cancel-portrait.png
var cancelPortraitPNG []byte

// cancellationSlateFrame은 현재 출력 크기에 맞는 취소 슬레이트를 만든다.
// 입력 비율을 별도로 분류하지 않고, 가로·세로 방향만으로 에셋을 고른 뒤
// 정확한 출력 해상도까지 리사이즈한다. 생성한 프레임의 보관 수명은
// RTMPEgress가 관리하므로, 서로 다른 입력 규격이 들어와도 전역 캐시가 누적되지
// 않는다.
func cancellationSlateFrame(width, height uint16, format config.WireFormat) (frame, error) {
	if width == 0 || height == 0 {
		return frame{}, fmt.Errorf("invalid cancellation slate size %dx%d", width, height)
	}
	if format == "" {
		format = config.WireFormatJPEG
	}

	asset := cancellationSlateAsset(width, height)
	source, err := png.Decode(bytes.NewReader(asset))
	if err != nil {
		return frame{}, fmt.Errorf("decode cancellation slate: %w", err)
	}

	resized := image.NewRGBA(image.Rect(0, 0, int(width), int(height)))
	draw.CatmullRom.Scale(resized, resized.Bounds(), source, source.Bounds(), draw.Src, nil)

	result := frame{width: width, height: height}
	switch format {
	case config.WireFormatJPEG:
		var encoded bytes.Buffer
		if err := jpeg.Encode(&encoded, resized, &jpeg.Options{Quality: 90}); err != nil {
			return frame{}, fmt.Errorf("encode cancellation slate as JPEG: %w", err)
		}
		result.data = encoded.Bytes()
	case config.WireFormatRaw:
		result.data = rgbaToYUV420P(resized)
	default:
		return frame{}, fmt.Errorf("unsupported cancellation slate wire format %q", format)
	}
	return result, nil
}

func cancellationSlateAsset(width, height uint16) []byte {
	if width >= height {
		return cancelLandscapePNG
	}
	return cancelPortraitPNG
}

// rgbaToYUV420P는 raw egress 테스트 경로까지 지원하기 위한 변환이다. 실제
// 서버 설정은 JPEG만 허용하지만, raw 모드에서도 슬레이트 프레임의 크기와
// 픽셀 형식을 기존 egress 입력과 일치시킨다.
func rgbaToYUV420P(source *image.RGBA) []byte {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	chromaWidth, chromaHeight := (width+1)/2, (height+1)/2
	result := make([]byte, width*height+2*chromaWidth*chromaHeight)
	uOffset := width * height
	vOffset := uOffset + chromaWidth*chromaHeight

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			red, green, blue, _ := source.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			result[y*width+x] = limitedRangeLuma(uint8(red>>8), uint8(green>>8), uint8(blue>>8))
		}
	}
	for chromaY := 0; chromaY < chromaHeight; chromaY++ {
		for chromaX := 0; chromaX < chromaWidth; chromaX++ {
			var red, green, blue, count int
			for y := chromaY * 2; y < minInt(chromaY*2+2, height); y++ {
				for x := chromaX * 2; x < minInt(chromaX*2+2, width); x++ {
					r, g, b, _ := source.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
					red += int(r >> 8)
					green += int(g >> 8)
					blue += int(b >> 8)
					count++
				}
			}
			red /= count
			green /= count
			blue /= count
			index := chromaY*chromaWidth + chromaX
			result[uOffset+index] = limitedRangeChromaU(red, green, blue)
			result[vOffset+index] = limitedRangeChromaV(red, green, blue)
		}
	}
	return result
}

func limitedRangeLuma(red, green, blue uint8) uint8 {
	return uint8((66*int(red)+129*int(green)+25*int(blue)+128)>>8 + 16)
}

func limitedRangeChromaU(red, green, blue int) uint8 {
	return uint8((-38*red-74*green+112*blue+128)>>8 + 128)
}

func limitedRangeChromaV(red, green, blue int) uint8 {
	return uint8((112*red-94*green-18*blue+128)>>8 + 128)
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
