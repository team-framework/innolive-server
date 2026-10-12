package config

import "testing"

// TestFaceAnonymizationDefaultsToBlur: env를 주지 않은 배포는 종전 blur를 그대로 쓴다.
// 합성은 AI 서버에 모델·프리셋이 준비된 배포에서만 켠다.
func TestFaceAnonymizationDefaultsToBlur(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AIFaceAnonymization != FaceAnonymizationBlur {
		t.Fatalf("AIFaceAnonymization = %q, want blur", cfg.AIFaceAnonymization)
	}
}

func TestFaceAnonymizationFromEnv(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("AI_FACE_ANONYMIZATION", "face_swap")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AIFaceAnonymization != FaceAnonymizationFaceSwap {
		t.Fatalf("AIFaceAnonymization = %q, want face_swap", cfg.AIFaceAnonymization)
	}
}

// TestValidateRejectsUnknownFaceAnonymization: 오타가 기동 시점에 걸려야 한다.
// 런타임에는 조용히 blur로 동작해 합성이 꺼진 줄 모른다.
func TestValidateRejectsUnknownFaceAnonymization(t *testing.T) {
	cfg := validConfig()
	cfg.AIFaceAnonymization = "swap"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an unknown face anonymization to be rejected")
	}
}
