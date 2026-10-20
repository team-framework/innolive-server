package session

import (
	"errors"
	"testing"
	"time"

	"inno-live-server/internal/ai"

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

// 합성 워커가 모두 응답 불가면 합성 세션을 만들지 않는다(#507). 만들면 처음부터
// fail-closed 블러로 잠긴다. 블러 세션은 영향받지 않는다.
func TestFaceSwapRejectedWhenNoFaceSwapWorkerIsReady(t *testing.T) {
	manager := newTestManager(t, 0)
	manager.cfg.AIFaceSwapEnabled = true
	pool, err := ai.NewPool([]string{"a:1", "swap:1"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := pool.SetFaceSwapTargets([]string{"swap:1"}); err != nil {
		t.Fatal(err)
	}
	manager.ai = pool
	pool.SetTargetReady("swap:1", false)
	before, _ := manager.Capacity()

	if _, _, err := manager.CreateForUserWithFaceAnonymization(uuid.New(), DefaultProvider, "", "", FaceAnonymizationFaceSwap, nil); !errors.Is(err, ErrFaceSwapWorkerUnavailable) {
		t.Fatalf("face swap with no ready worker error = %v, want ErrFaceSwapWorkerUnavailable", err)
	}
	if after, _ := manager.Capacity(); after != before {
		t.Fatal("rejected face swap request consumed a session")
	}
	if _, _, err := manager.CreateForUserWithFaceAnonymization(uuid.New(), DefaultProvider, "", "", FaceAnonymizationBlur, nil); err != nil {
		t.Fatalf("blur session with a ready blur worker: %v", err)
	}

	pool.SetTargetReady("swap:1", true)
	if _, _, err := manager.CreateForUserWithFaceAnonymization(uuid.New(), DefaultProvider, "", "", FaceAnonymizationFaceSwap, nil); err != nil {
		t.Fatalf("face swap after the worker recovered: %v", err)
	}
}
