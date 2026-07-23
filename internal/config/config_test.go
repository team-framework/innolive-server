package config

import (
	"testing"
	"time"
)

func TestLoadAudioEgressFlags(t *testing.T) {
	t.Setenv("ENABLE_AUDIO_EGRESS", "true")
	t.Setenv("EGRESS_LATENCY_LOG", "true")
	t.Setenv("EGRESS_AUDIO_OFFSET_MS", "200")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.EnableAudioEgress {
		t.Error("EnableAudioEgress should be true")
	}
	if !cfg.EgressLatencyLog {
		t.Error("EgressLatencyLog should be true")
	}
	if cfg.EgressAudioOffset != 200*time.Millisecond {
		t.Errorf("EgressAudioOffset = %v, want 200ms", cfg.EgressAudioOffset)
	}
}

func TestLoadAudioEgressDefaultsOff(t *testing.T) {
	t.Setenv("ENABLE_AUDIO_EGRESS", "")
	t.Setenv("EGRESS_LATENCY_LOG", "")
	t.Setenv("EGRESS_AUDIO_OFFSET_MS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.EnableAudioEgress {
		t.Error("EnableAudioEgress should default to false")
	}
	if cfg.EgressLatencyLog {
		t.Error("EgressLatencyLog should default to false")
	}
	if cfg.EgressAudioOffset != 0 {
		t.Errorf("EgressAudioOffset = %v, want 0", cfg.EgressAudioOffset)
	}
}

// The offset is signed so tuning can shift audio either way.
func TestLoadAudioOffsetAcceptsNegative(t *testing.T) {
	t.Setenv("EGRESS_AUDIO_OFFSET_MS", "-50")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.EgressAudioOffset != -50*time.Millisecond {
		t.Errorf("EgressAudioOffset = %v, want -50ms", cfg.EgressAudioOffset)
	}
}

func TestValidateRejectsUnknownPrivacyMode(t *testing.T) {
	cfg := validConfig()
	cfg.PrivacyMode = "unknown"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected unknown privacy mode to be rejected")
	}
}

func TestValidateAllowsAllTestPlanModes(t *testing.T) {
	for _, mode := range []PrivacyMode{PrivacyModeBypass, PrivacyModeFixedDelay, PrivacyModeReal} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := validConfig()
			cfg.PrivacyMode = mode
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestValidateRequiresFFmpegInAllModes(t *testing.T) {
	for _, mode := range []PrivacyMode{PrivacyModeBypass, PrivacyModeFixedDelay, PrivacyModeReal} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := validConfig()
			cfg.PrivacyMode = mode
			cfg.FFmpegPath = ""
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected an empty FFMPEG_PATH to be rejected")
			}
		})
	}
}

func TestLoadAcceptsPythonServerDelayAlias(t *testing.T) {
	t.Setenv("AI_PRIVACY_MODE", "fixed_delay")
	t.Setenv("AI_PRIVACY_FIXED_DELAY", "")
	t.Setenv("AI_PRIVACY_FIXED_DELAY_MS", "12.5")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrivacyFixedDelay != 12500*time.Microsecond {
		t.Fatalf("PrivacyFixedDelay = %s", cfg.PrivacyFixedDelay)
	}
}

func TestSplitURLsCanDisableDefaultSTUNExplicitly(t *testing.T) {
	if urls := splitURLs("none"); len(urls) != 0 {
		t.Fatalf("splitURLs(none) = %v", urls)
	}
}

func validConfig() Config {
	return Config{
		HTTPAddr:           ":8000",
		PrivacyMode:        PrivacyModeReal,
		PrivacyFixedDelay:  20 * time.Millisecond,
		AIAddress:          "localhost:50051",
		FFmpegPath:         "ffmpeg",
		AITimeout:          time.Second,
		AIPreflightTimeout: time.Second,
		UDPPortMin:         50000,
		UDPPortMax:         60000,
		FrameQueueSize:     2,
	}
}
