package session

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// 얼굴 가림 방식은 세션마다 고른다. 생략하면 blur이고, 합성은 켜진 배포의 서버
// 처리 세션에서만 받는다.
func TestFaceAnonymizationIsChosenPerSession(t *testing.T) {
	manager := newTestManager(t, 0)
	manager.cfg.AIFaceSwapEnabled = true

	blur, _, err := manager.CreateForUserWithFaceAnonymization(uuid.New(), DefaultProvider, "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	swap, _, err := manager.CreateForUserWithFaceAnonymization(uuid.New(), DefaultProvider, "", "", FaceAnonymizationFaceSwap, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := blur.Response().FaceAnonymization; got != FaceAnonymizationBlur {
		t.Fatalf("default face_anonymization = %q, want blur", got)
	}
	if got := swap.Response().FaceAnonymization; got != FaceAnonymizationFaceSwap {
		t.Fatalf("face_anonymization = %q, want face_swap", got)
	}
}

func TestFaceSwapRejectedWhenUnavailableOrInvalid(t *testing.T) {
	manager := newTestManager(t, 0)
	before, _ := manager.Capacity()

	if _, _, err := manager.CreateForUserWithFaceAnonymization(uuid.New(), DefaultProvider, "", "", FaceAnonymizationFaceSwap, nil); !errors.Is(err, ErrFaceSwapUnavailable) {
		t.Fatalf("disabled face swap error = %v, want ErrFaceSwapUnavailable", err)
	}
	manager.cfg.AIFaceSwapEnabled = true
	if _, _, err := manager.CreateForUserWithFaceAnonymization(uuid.New(), DefaultProvider, AIProcessingOnDevice, "", FaceAnonymizationFaceSwap, nil); !errors.Is(err, ErrInvalidFaceAnonymization) {
		t.Fatalf("on-device face swap error = %v, want ErrInvalidFaceAnonymization", err)
	}
	if _, _, err := manager.CreateForUserWithFaceAnonymization(uuid.New(), DefaultProvider, "", "", "pixelate", nil); !errors.Is(err, ErrInvalidFaceAnonymization) {
		t.Fatalf("unknown mode error = %v, want ErrInvalidFaceAnonymization", err)
	}
	if after, _ := manager.Capacity(); after != before {
		t.Fatal("rejected request consumed a session")
	}
}
