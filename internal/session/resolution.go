package session

import "errors"

// 송출 해상도는 세션 생성 시 정해지고 세션 중 바뀌지 않는다(#271). 디코더가
// 트랙 도착 즉시 이 해상도로 핀을 걸고 뜨므로, 방송 준비(prepare) 시점에는
// 이미 늦다 — 하위 단계가 세션 내 치수 불변을 전제로 한다.
const (
	Resolution720p = "720p"
	ResolutionFHD  = "fhd"

	// fhdPinLongEdge는 FHD 세션의 디코더 장변이다. AI 서버 MAX_LONG_EDGE와 같다.
	fhdPinLongEdge = 1920
)

var ErrInvalidResolution = errors.New("broadcast_resolution must be 720p or fhd")

// normalizeResolution은 빈 값을 720p로 채우고 미지원 값을 거절한다 — 해상도를
// 보내지 않는 기존 클라이언트는 종전대로 720p다.
func normalizeResolution(value string) (string, error) {
	switch value {
	case "", Resolution720p:
		return Resolution720p, nil
	case ResolutionFHD:
		return ResolutionFHD, nil
	default:
		return "", ErrInvalidResolution
	}
}

// pinLongEdgeFor는 세션의 디코더 장변을 정한다. 전역 핀이 꺼져 있으면(0,
// 로컬·벤치) 종전대로 핀을 걸지 않는다. 켜져 있으면 720p는 그 값(프로덕션
// 1280), FHD는 1920이다.
func pinLongEdgeFor(resolution string, globalPin int) int {
	if globalPin == 0 {
		return 0
	}
	if resolution == ResolutionFHD {
		return fhdPinLongEdge
	}
	return globalPin
}
