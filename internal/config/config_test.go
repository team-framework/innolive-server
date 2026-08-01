package config

import (
	"testing"
	"time"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()

	t.Setenv(
		"DATABASE_URL",
		"postgres://test:test@localhost:5432/test?sslmode=disable",
	)
}

func TestLoadAudioEgressFlags(t *testing.T) {
	setRequiredEnv(t)

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
	setRequiredEnv(t)

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
	setRequiredEnv(t)

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

func TestValidateEgressVideoBitrate(t *testing.T) {
	for _, valid := range []string{"", "2500k", "5000K", "6M", "4500000"} {
		cfg := validConfig()
		cfg.EgressVideoBitrate = valid
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() with EgressVideoBitrate=%q error = %v", valid, err)
		}
	}
	for _, invalid := range []string{"fast", "k", "2500kk", "-100k"} {
		cfg := validConfig()
		cfg.EgressVideoBitrate = invalid
		if err := cfg.Validate(); err == nil {
			t.Errorf("expected EgressVideoBitrate=%q to be rejected", invalid)
		}
	}
}

func TestLoadAcceptsPythonServerDelayAlias(t *testing.T) {
	setRequiredEnv(t)

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

func TestLoadDatabaseConfig(t *testing.T) {
	databaseURL :=
		"postgres://test:test@localhost:5432/test?sslmode=disable"

	t.Setenv("DATABASE_URL", databaseURL)
	t.Setenv("DATABASE_MAX_OPEN_CONNS", "20")
	t.Setenv("DATABASE_MAX_IDLE_CONNS", "8")
	t.Setenv("DATABASE_CONN_MAX_LIFETIME", "45m")
	t.Setenv("DATABASE_CONN_MAX_IDLE_TIME", "10m")
	t.Setenv("DATABASE_MIGRATION_MODE", "versioned")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.DatabaseURL != databaseURL {
		t.Errorf(
			"DatabaseURL = %q, want %q",
			cfg.DatabaseURL,
			databaseURL,
		)
	}
	if cfg.DatabaseMaxOpenConns != 20 {
		t.Errorf(
			"DatabaseMaxOpenConns = %d, want 20",
			cfg.DatabaseMaxOpenConns,
		)
	}
	if cfg.DatabaseMaxIdleConns != 8 {
		t.Errorf(
			"DatabaseMaxIdleConns = %d, want 8",
			cfg.DatabaseMaxIdleConns,
		)
	}
	if cfg.DatabaseConnMaxLifetime != 45*time.Minute {
		t.Errorf(
			"DatabaseConnMaxLifetime = %v, want 45m",
			cfg.DatabaseConnMaxLifetime,
		)
	}
	if cfg.DatabaseConnMaxIdleTime != 10*time.Minute {
		t.Errorf(
			"DatabaseConnMaxIdleTime = %v, want 10m",
			cfg.DatabaseConnMaxIdleTime,
		)
	}
	if cfg.DatabaseMigrationMode != DatabaseMigrationModeVersioned {
		t.Errorf(
			"DatabaseMigrationMode = %q, want %q",
			cfg.DatabaseMigrationMode,
			DatabaseMigrationModeVersioned,
		)
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

		DatabaseURL:             "postgres://test:test@localhost:5432/test?sslmode=disable",
		DatabaseMaxOpenConns:    10,
		DatabaseMaxIdleConns:    5,
		DatabaseConnMaxLifetime: 30 * time.Minute,
		DatabaseConnMaxIdleTime: 5 * time.Minute,

		DatabaseMigrationMode: DatabaseMigrationModeAuto,
	}
}
