package media

import (
	"fmt"
	"image"
	"math"
)

// decoderPinMinDimension은 핀이 만들어낼 수 있는 가장 짧은 변이다. AI 서버가
// 이보다 작은 프레임을 거부하므로(innolive-ai service/frame.py의
// MIN_FRAME_DIMENSION) 극단적인 종횡비에서도 그 아래로 내려가지 않는다.
const decoderPinMinDimension = 32

// pinDimensions는 소스 치수와 장변 상한으로 디코더 출력 치수를 정한다.
//
// 방향을 특정하지 않는다 — 긴 변을 longEdge에 맞추고 짧은 변은 소스 종횡비
// 그대로 유도하므로, 가로든 세로든 어떤 비율이든 여백 없이 담긴다. 프로덕션
// 세션의 다수가 세로라 단일 WxH 고정은 화면 대부분을 여백으로 채운다(#122).
//
// 구간의 첫 키프레임만 보고 정해도 되는 이유는 WebRTC 램프업이 프레임을
// 축소할 뿐 회전시키지는 않기 때문이다 — 해상도는 못 믿어도 종횡비는 믿을 수
// 있다. 방향이 실제로 바뀌면(앱이 세로로 시작했다 가로로 바꾸는 경우 등)
// 디코더가 그 키프레임부터 새로 핀을 정한다(#449).
//
// yuv420p가 짝수 치수를 요구하므로 양쪽을 짝수로 맞춘다. 그 보정으로 생기는
// 비율 오차는 관측된 해상도에서 0이고 최악의 경우에도 0.3% 미만이며,
// pinFilter의 pad가 흡수한다.
func pinDimensions(sourceWidth, sourceHeight, longEdge uint16) (uint16, uint16, error) {
	if sourceWidth == 0 || sourceHeight == 0 {
		return 0, 0, fmt.Errorf("decoder pin: source is %dx%d", sourceWidth, sourceHeight)
	}
	if longEdge < decoderPinMinDimension {
		return 0, 0, fmt.Errorf("decoder pin: long edge %d is below %d", longEdge, decoderPinMinDimension)
	}
	portrait := sourceHeight > sourceWidth
	long, short := sourceWidth, sourceHeight
	if portrait {
		long, short = sourceHeight, sourceWidth
	}
	scaled := int(math.Round(float64(short) * float64(longEdge) / float64(long)))
	if scaled < decoderPinMinDimension {
		scaled = decoderPinMinDimension
	}
	pinnedShort := evenDown(scaled + scaled%2)
	pinnedLong := evenDown(int(longEdge) + int(longEdge)%2)
	if portrait {
		return pinnedShort, pinnedLong, nil
	}
	return pinnedLong, pinnedShort, nil
}

// evenDown은 짝수로 올린 값이 uint16을 넘어가지 않게 잘라낸다. Validate가
// 장변을 훨씬 낮게 제한하므로 실제로는 걸리지 않지만, helper 단독으로도
// 안전하도록 둔다.
func evenDown(value int) uint16 {
	if value > math.MaxUint16 {
		return math.MaxUint16 - 1
	}
	return uint16(value)
}

// pinFilter는 디코더 출력을 핀 치수로 고정하는 -vf 체인을 만든다.
// force_original_aspect_ratio=decrease와 pad를 함께 쓰므로 입력 비율이
// 핀과 약간 어긋나도 잘리지 않고 레터박스로 흡수된다. 방향 전환은 디코더를
// 다시 띄워 처리하므로 여기까지 오지 않는다(#449).
func pinFilter(width, height uint16) string {
	return fmt.Sprintf(
		"scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:-1:-1",
		width, height, width, height,
	)
}

// inputZoomFactor는 클라이언트 입력을 출력에서 확대해 보여주는 배율이다.
// AI에는 원본 그대로 보내고, AI가 블러를 입혀 돌려준 프레임을 미리보기·송출
// 인코더가 각각 확대한다. 블러가 이미 프레임에 들어가 있으므로 확대해도 위치가
// 어긋나지 않는다.
const inputZoomFactor = 1.1

// zoomCropFilter는 입력 중앙을 1/inputZoomFactor 크기로 잘라내는 -vf 단계다.
// 뒤따르는 scale이 출력 치수로 다시 키우면서 확대가 된다. crop의 x·y 기본값이
// 중앙이고, yuv420p가 짝수 치수를 요구하므로 잘라낼 크기를 짝수로 내린다.
func zoomCropFilter() string {
	return fmt.Sprintf("crop=trunc(iw/%[1]g/2)*2:trunc(ih/%[1]g/2)*2", inputZoomFactor)
}

// zoomCropRect는 zoomCropFilter가 width×height 프레임에서 남기는 영역이다.
// ffmpeg crop과 같은 순서로 double 연산 후 버리고, yuv420p 색차 정렬에 맞춰
// 시작 좌표를 짝수로 내린다.
func zoomCropRect(width, height uint16) image.Rectangle {
	cropWidth := int(float64(width)/inputZoomFactor/2) * 2
	cropHeight := int(float64(height)/inputZoomFactor/2) * 2
	x := ((int(width) - cropWidth) / 2) &^ 1
	y := ((int(height) - cropHeight) / 2) &^ 1
	return image.Rect(x, y, x+cropWidth, y+cropHeight)
}

// zoomFilter는 중앙을 잘라낸 뒤 width×height로 되돌려 출력 치수를 유지하는
// -vf 체인이다. 치수를 모르면(0) 잘라낸 크기를 같은 배율로 키운다.
func zoomFilter(width, height uint16) string {
	if width == 0 || height == 0 {
		return fmt.Sprintf("%s,scale=trunc(iw*%[2]g/2)*2:trunc(ih*%[2]g/2)*2", zoomCropFilter(), inputZoomFactor)
	}
	return fmt.Sprintf("%s,scale=%d:%d", zoomCropFilter(), width, height)
}
