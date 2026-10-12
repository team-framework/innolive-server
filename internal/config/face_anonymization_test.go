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
