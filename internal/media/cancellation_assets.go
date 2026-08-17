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
// 입력 비율을 별도로 분류하지 않고, 가로·세로 방향만으로 에셋을 고른다. 에셋의
// 중앙을 출력 비율에 맞게 잘라 리사이즈하므로 로고·문구의 비율을 유지하면서도
// RTMP 출력 프레임 전체를 채운다. 생성한 프레임의 보관 수명은 RTMPEgress가
// 관리하므로, 서로 다른 입력 규격이 들어와도 전역 캐시가 누적되지 않는다.
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

	resized := renderCancellationSlate(source, width, height)

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

// cancellationSlateCropRect는 에셋의 중앙에서 목표 비율만큼을 선택한다. crop 뒤
// 리사이즈하면 가로·세로 배율이 같아져 stretch 왜곡 없이 출력 프레임을 채운다.
func cancellationSlateCropRect(source image.Rectangle, targetWidth, targetHeight uint16) image.Rectangle {
	sourceWidth, sourceHeight := source.Dx(), source.Dy()
	if int(targetWidth)*sourceHeight > int(targetHeight)*sourceWidth {
		cropHeight := sourceWidth * int(targetHeight) / int(targetWidth)
		cropY := source.Min.Y + (sourceHeight-cropHeight)/2
		return image.Rect(source.Min.X, cropY, source.Max.X, cropY+cropHeight)
	}
	if int(targetWidth)*sourceHeight < int(targetHeight)*sourceWidth {
		cropWidth := sourceHeight * int(targetWidth) / int(targetHeight)
		cropX := source.Min.X + (sourceWidth-cropWidth)/2
		return image.Rect(cropX, source.Min.Y, cropX+cropWidth, source.Max.Y)
	}
	return source
}

// renderCancellationSlate는 중앙 crop과 리사이즈를 하나의 경로로 수행한다. 이
// helper를 분리해 테스트가 실제 렌더링 과정에서 crop 영역이 사용되는지 검증할 수
// 있게 한다.
func renderCancellationSlate(source image.Image, width, height uint16) *image.RGBA {
	resized := image.NewRGBA(image.Rect(0, 0, int(width), int(height)))
	draw.CatmullRom.Scale(resized, resized.Bounds(), source, cancellationSlateCropRect(source.Bounds(), width, height), draw.Src, nil)
	return resized
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
