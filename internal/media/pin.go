package media

import (
	"fmt"
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
// 첫 프레임만 보고 정해도 되는 이유는 WebRTC 램프업이 프레임을 축소할 뿐
// 회전시키지는 않기 때문이다 — 첫 프레임은 해상도를 못 믿어도 종횡비는
// 믿을 수 있다.
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
// 핀과 어긋나도 잘리지 않고 레터박스로 흡수된다 — 방송 도중 기기를
// 회전시키는 경우가 여기 걸린다.
func pinFilter(width, height uint16) string {
	return fmt.Sprintf(
		"scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:-1:-1",
		width, height, width, height,
	)
}
