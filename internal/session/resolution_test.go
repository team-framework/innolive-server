package session

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestPinLongEdgeFor(t *testing.T) {
	tests := []struct {
		resolution string
		globalPin  int
		want       int
	}{
		{Resolution720p, 1280, 1280},
		{ResolutionFHD, 1280, 1920},
		// 전역 핀이 꺼진 배포(로컬·벤치)는 해상도와 무관하게 종전대로 핀 없음.
		{Resolution720p, 0, 0},
		{ResolutionFHD, 0, 0},
	}
	for _, test := range tests {
		if got := pinLongEdgeFor(test.resolution, test.globalPin); got != test.want {
			t.Fatalf("pinLongEdgeFor(%q, %d) = %d, want %d", test.resolution, test.globalPin, got, test.want)
		}
	}
}

func TestCreateFixesResolutionPerSession(t *testing.T) {
	manager := newTestManager(t, 0)
	manager.cfg.DecoderPinLongEdge = 1280

	fhd, _, err := manager.CreateForUserWithResolution(uuid.New(), DefaultProvider, "", ResolutionFHD, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy, _, err := manager.CreateForUserWithAIProcessing(uuid.New(), DefaultProvider, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fhd.BroadcastResolution != ResolutionFHD || fhd.pinLongEdge != 1920 {
		t.Fatalf("fhd session = %q pin %d", fhd.BroadcastResolution, fhd.pinLongEdge)
	}
	// 해상도를 보내지 않는 기존 클라이언트는 720p다.
	if legacy.BroadcastResolution != Resolution720p || legacy.pinLongEdge != 1280 {
		t.Fatalf("legacy session = %q pin %d", legacy.BroadcastResolution, legacy.pinLongEdge)
	}
	if got := fhd.Response().BroadcastResolution; got != ResolutionFHD {
		t.Fatalf("response resolution = %q", got)
	}
}

func TestCreateRejectsUnknownResolution(t *testing.T) {
	manager := newTestManager(t, 1)
	for _, value := range []string{"1080p", "FHD", "4k"} {
		if _, _, err := manager.CreateForUserWithResolution(uuid.New(), DefaultProvider, "", value, nil); !errors.Is(err, ErrInvalidResolution) {
			t.Fatalf("%q: error = %v, want ErrInvalidResolution", value, err)
		}
	}
	if active, _ := manager.Capacity(); active != 0 {
		t.Fatalf("rejected creates left %d sessions", active)
	}
}
