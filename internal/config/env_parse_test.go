package config

import (
	"strings"
	"testing"
)

func TestLoadRejectsMalformedEnvValues(t *testing.T) {
	cases := []struct {
		key   string
		value string
	}{
		{"MAX_EGRESS_SLOTS", `"12"`},
		{"AI_PREFLIGHT_REQUIRED", "yes"},
		{"AI_GRPC_TIMEOUT", "5"},
		{"AI_PRIVACY_FIXED_DELAY", "20"},
		{"AI_PRIVACY_FIXED_DELAY_MS", "twenty"},
		{"WEBRTC_DISCONNECTED_GRACE_SECONDS", "ten"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(tc.key, tc.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted %s=%q", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not name %s", err, tc.key)
			}
			if strings.Contains(err.Error(), tc.value) {
				t.Errorf("error %q exposes the raw value", err)
			}
		})
	}
}

func TestLoadAcceptsWellFormedEnvValues(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MAX_EGRESS_SLOTS", " 12 ")
	t.Setenv("AI_PREFLIGHT_REQUIRED", "true")
	t.Setenv("AI_GRPC_TIMEOUT", "7s")
	t.Setenv("WEBRTC_DISCONNECTED_GRACE", "")
	t.Setenv("WEBRTC_DISCONNECTED_GRACE_SECONDS", "2.5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MaxEgressSlots != 12 || !cfg.AIPreflightRequired || cfg.AITimeout.String() != "7s" ||
		cfg.DisconnectedGracePeriod.String() != "2.5s" {
		t.Errorf("parsed config = slots %d, preflight %v, timeout %s, grace %s",
			cfg.MaxEgressSlots, cfg.AIPreflightRequired, cfg.AITimeout, cfg.DisconnectedGracePeriod)
	}
}
