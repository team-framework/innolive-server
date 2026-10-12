package media

import (
	"bytes"
	"strings"
	"testing"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

func face(whitelisted bool) *aiv1.FaceMetadata {
	return &aiv1.FaceMetadata{ClassName: "face", Whitelisted: whitelisted}
}

func plate(whitelisted bool) *aiv1.FaceMetadata {
	return &aiv1.FaceMetadata{ClassName: numberPlateClassName, Whitelisted: whitelisted}
}

// TestClassifyMosaicFollowsAIBlurRule은 분류가 AI 블러 규칙과 같은지 지킨다.
// 번호판은 화이트리스트여도 블러하고, 화이트리스트 얼굴은 블러하지 않는다.
func TestClassifyMosaicFollowsAIBlurRule(t *testing.T) {
	tests := []struct {
		name    string
		objects []*aiv1.FaceMetadata
		want    mosaicCounts
		kind    string
	}{
		{name: "no objects", objects: nil, want: mosaicCounts{}, kind: MosaicKindNone},
		{name: "face only", objects: []*aiv1.FaceMetadata{face(false), face(false)}, want: mosaicCounts{faces: 2}, kind: MosaicKindFaceOnly},
		{name: "plate only", objects: []*aiv1.FaceMetadata{plate(false)}, want: mosaicCounts{plates: 1}, kind: MosaicKindPlateOnly},
		{name: "both", objects: []*aiv1.FaceMetadata{face(false), plate(false)}, want: mosaicCounts{faces: 1, plates: 1}, kind: MosaicKindBoth},
		{name: "whitelisted face is not blurred", objects: []*aiv1.FaceMetadata{face(true)}, want: mosaicCounts{whitelistedFaces: 1}, kind: MosaicKindNone},
		{name: "whitelisted plate is still blurred", objects: []*aiv1.FaceMetadata{plate(true), face(true)}, want: mosaicCounts{plates: 1, whitelistedFaces: 1}, kind: MosaicKindPlateOnly},
		{name: "empty class is a face", objects: []*aiv1.FaceMetadata{{}}, want: mosaicCounts{faces: 1}, kind: MosaicKindFaceOnly},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyMosaic(test.objects)
			if got != test.want {
				t.Fatalf("classifyMosaic() = %+v, want %+v", got, test.want)
			}
			if got.kind() != test.kind {
				t.Fatalf("kind() = %q, want %q", got.kind(), test.kind)
			}
		})
	}
}

func TestMosaicTallyAccumulatesFramesAndObjects(t *testing.T) {
	tally := NewMosaicTally()
	tally.record(classifyMosaic([]*aiv1.FaceMetadata{face(false), face(false)}))
	tally.record(classifyMosaic([]*aiv1.FaceMetadata{face(false), plate(false)}))
	tally.record(classifyMosaic([]*aiv1.FaceMetadata{plate(true)}))
	tally.record(classifyMosaic([]*aiv1.FaceMetadata{face(true)}))

	want := MosaicSummary{
		FaceOnlyFrames:         1,
		PlateOnlyFrames:        1,
		BothFrames:             1,
		NoneFrames:             1,
		FaceObjects:            3,
		PlateObjects:           2,
		WhitelistedFaceObjects: 1,
	}
	if got := tally.Summary(); got != want {
		t.Fatalf("Summary() = %+v, want %+v", got, want)
	}
}

// TestProcessorRecordsMosaicOnlyForSuccessfulFrames는 AI 처리에 성공한 프레임만
// 서버 합계와 세션 누계에 더하는지 지킨다. 실패 프레임은 블러 여부를 알 수 없다.
func TestProcessorRecordsMosaicOnlyForSuccessfulFrames(t *testing.T) {
	fail := false
	ai := &fakeAIStream{process: func(data []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
		if fail {
			return &aiv1.ProcessedVideoChunk{Data: []byte("out"), Timestamp: timestamp, ErrorCode: "decode_failed"}, nil
		}
		return &aiv1.ProcessedVideoChunk{
			Data:          []byte("out"),
			Timestamp:     timestamp,
			StatusMessage: "success",
			Faces:         []*aiv1.FaceMetadata{face(false), plate(false)},
		}, nil
	}}
	registry := metrics.New()
	processor, err := NewProcessor(config.PrivacyModeReal, 0, ai, registry, nil, config.WireFormatJPEG, config.FailurePolicyFreeze, 0)
	if err != nil {
		t.Fatal(err)
	}
	tally := NewMosaicTally()
	processor.SetMosaicTally(tally)

	if _, err := processor.ProcessImage([]byte("in"), 1, 64, 48); err != nil {
		t.Fatalf("ProcessImage() error = %v", err)
	}
	fail = true
	if _, err := processor.ProcessImage([]byte("in"), 2, 64, 48); err == nil {
		t.Fatal("ProcessImage() error = nil, want failure for error_code")
	}

	want := MosaicSummary{BothFrames: 1, FaceObjects: 1, PlateObjects: 1}
	if got := tally.Summary(); got != want {
		t.Fatalf("Summary() = %+v, want %+v", got, want)
	}
	var output bytes.Buffer
	registry.WritePrometheus(&output)
	for _, line := range []string{
		`innolive_mosaic_frames_total{kind="both"} 1`,
		`innolive_mosaic_objects_total{object="face"} 1`,
		`innolive_mosaic_objects_total{object="number_plate"} 1`,
		`innolive_mosaic_objects_total{object="whitelisted_face"} 0`,
	} {
		if !strings.Contains(output.String(), line) {
			t.Errorf("metrics output missing %q", line)
		}
	}
}
