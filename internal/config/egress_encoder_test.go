package config

import "testing"

// TestEgressVideoEncoderDefaultsToX264: env를 주지 않은 배포는 종전 CPU
// 인코딩을 그대로 쓴다. NVENC는 GPU가 노출된 배포에서만 켠다.
func TestEgressVideoEncoderDefaultsToX264(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EgressVideoEncoder != EgressVideoEncoderX264 {
		t.Fatalf("EgressVideoEncoder = %q, want x264", cfg.EgressVideoEncoder)
	}
	if cfg.EgressNVENCGPUs != 0 {
		t.Fatalf("EgressNVENCGPUs = %d, want 0", cfg.EgressNVENCGPUs)
	}
}

func TestEgressVideoEncoderFromEnv(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("EGRESS_VIDEO_ENCODER", "nvenc")
	t.Setenv("EGRESS_NVENC_GPUS", "2")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EgressVideoEncoder != EgressVideoEncoderNVENC || cfg.EgressNVENCGPUs != 2 {
		t.Fatalf("encoder = %q, gpus = %d", cfg.EgressVideoEncoder, cfg.EgressNVENCGPUs)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("nvenc configuration must validate: %v", err)
	}
}

// TestValidateRejectsUnknownEgressEncoder: 오타가 기동 시점에 걸려야 한다.
// 런타임에 걸리면 첫 송출에서 FFmpeg가 죽는다.
func TestValidateRejectsUnknownEgressEncoder(t *testing.T) {
	cfg := validConfig()
	cfg.EgressVideoEncoder = "nvidia"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an unknown egress encoder to be rejected")
	}
}

func TestValidateRejectsNegativeNVENCGPUs(t *testing.T) {
	cfg := validConfig()
	cfg.EgressNVENCGPUs = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected a negative GPU count to be rejected")
	}
}
