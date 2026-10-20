package config

import "testing"

// TestFaceSwapDisabledByDefault: env를 주지 않은 배포는 합성을 고를 수 없다.
// 합성은 AI 서버에 모델·프리셋이 준비된 배포에서만 켠다.
func TestFaceSwapDisabledByDefault(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AIFaceSwapEnabled {
		t.Fatal("AIFaceSwapEnabled = true, want false")
	}
}

func TestFaceSwapEnabledFromEnv(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("AI_FACE_SWAP_ENABLED", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AIFaceSwapEnabled {
		t.Fatal("AIFaceSwapEnabled = false, want true")
	}
}

// 합성 전용 워커는 AI_GRPC_TARGETS 중 일부로 지정한다(#507).
func TestFaceSwapTargetsFromEnv(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("AI_GRPC_TARGETS", "127.0.0.1:50051,127.0.0.1:50052")
	t.Setenv("AI_FACE_SWAP_TARGETS", " 127.0.0.1:50052 ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AIFaceSwapTargets) != 1 || cfg.AIFaceSwapTargets[0] != "127.0.0.1:50052" {
		t.Fatalf("AIFaceSwapTargets = %v, want [127.0.0.1:50052]", cfg.AIFaceSwapTargets)
	}
}

func TestFaceSwapTargetsRejectInvalidLists(t *testing.T) {
	for name, swapTargets := range map[string]string{
		"outside AI_GRPC_TARGETS": "127.0.0.1:50053",
		"no blur worker left":     "127.0.0.1:50051,127.0.0.1:50052",
	} {
		t.Run(name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("AI_PRIVACY_MODE", "real")
			t.Setenv("AI_GRPC_TARGETS", "127.0.0.1:50051,127.0.0.1:50052")
			t.Setenv("AI_FACE_SWAP_TARGETS", swapTargets)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted AI_FACE_SWAP_TARGETS=%q", swapTargets)
			}
		})
	}
}
