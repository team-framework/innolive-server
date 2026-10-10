package media

import (
	"sync/atomic"

	aiv1 "inno-live-server/api/gen/aiv1"
)

// 프레임 분류 라벨이다. AI가 이 프레임에서 무엇을 블러했는지로 나눈다(#412).
const (
	MosaicKindFaceOnly  = "face_only"
	MosaicKindPlateOnly = "plate_only"
	MosaicKindBoth      = "both"
	MosaicKindNone      = "none"
)

// AI 서빙 계약의 번호판 클래스 이름이다. 그 밖의 클래스(빈 값 포함)는 AI가
// 얼굴로 다룬다.
const numberPlateClassName = "number_plate"

// mosaicCounts는 AI 응답 한 프레임의 객체 목록을 블러 기준으로 센 결과다.
type mosaicCounts struct {
	faces            int
	plates           int
	whitelistedFaces int
}

// classifyMosaic은 AI의 블러 규칙(service/mosaic.py `_protected_mask`)과 같은
// 기준으로 객체를 센다. 번호판은 화이트리스트와 상관없이 항상 블러하고, 얼굴은
// 화이트리스트에 있으면 블러하지 않는다.
func classifyMosaic(objects []*aiv1.FaceMetadata) mosaicCounts {
	var counts mosaicCounts
	for _, object := range objects {
		switch {
		case object.GetClassName() == numberPlateClassName:
			counts.plates++
		case object.GetWhitelisted():
			counts.whitelistedFaces++
		default:
			counts.faces++
		}
	}
	return counts
}

func (c mosaicCounts) kind() string {
	switch {
	case c.faces > 0 && c.plates > 0:
		return MosaicKindBoth
	case c.faces > 0:
		return MosaicKindFaceOnly
	case c.plates > 0:
		return MosaicKindPlateOnly
	default:
		return MosaicKindNone
	}
}

// MosaicTally는 세션 하나의 블러 분류 누계다. Prometheus 카운터는 세션 구분이
// 없어 동시 방송이 섞이므로, 방송 1회 단위 집계는 이 누계로 한다. 해상도 전환으로
// Processor가 바뀌어도 세션이 같은 누계를 넘겨 이어서 센다.
type MosaicTally struct {
	faceOnly         atomic.Uint64
	plateOnly        atomic.Uint64
	both             atomic.Uint64
	none             atomic.Uint64
	faces            atomic.Uint64
	plates           atomic.Uint64
	whitelistedFaces atomic.Uint64
}

// MosaicSummary는 MosaicTally의 한 시점 값이다. 객체 수는 프레임마다 다시 센
// 합이라, 같은 사람이 30프레임 동안 보이면 30으로 센다.
type MosaicSummary struct {
	FaceOnlyFrames         uint64
	PlateOnlyFrames        uint64
	BothFrames             uint64
	NoneFrames             uint64
	FaceObjects            uint64
	PlateObjects           uint64
	WhitelistedFaceObjects uint64
}

func NewMosaicTally() *MosaicTally {
	return &MosaicTally{}
}

func (t *MosaicTally) record(counts mosaicCounts) {
	switch counts.kind() {
	case MosaicKindBoth:
		t.both.Add(1)
	case MosaicKindFaceOnly:
		t.faceOnly.Add(1)
	case MosaicKindPlateOnly:
		t.plateOnly.Add(1)
	default:
		t.none.Add(1)
	}
	t.faces.Add(uint64(counts.faces))
	t.plates.Add(uint64(counts.plates))
	t.whitelistedFaces.Add(uint64(counts.whitelistedFaces))
}

func (t *MosaicTally) Summary() MosaicSummary {
	return MosaicSummary{
		FaceOnlyFrames:         t.faceOnly.Load(),
		PlateOnlyFrames:        t.plateOnly.Load(),
		BothFrames:             t.both.Load(),
		NoneFrames:             t.none.Load(),
		FaceObjects:            t.faces.Load(),
		PlateObjects:           t.plates.Load(),
		WhitelistedFaceObjects: t.whitelistedFaces.Load(),
	}
}
