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

func TestLoadWebRTCRecoveryDefaults(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.WebRTCRecoveryWindow != 50*time.Second {
		t.Fatalf("WebRTCRecoveryWindow = %s, want 50s", cfg.WebRTCRecoveryWindow)
	}
	if cfg.WebRTCRecoveryDebounce != 2*time.Second {
		t.Fatalf("WebRTCRecoveryDebounce = %s, want 2s", cfg.WebRTCRecoveryDebounce)
	}
	if cfg.WebRTCRecoveryAttempts != 10 {
		t.Fatalf("WebRTCRecoveryAttempts = %d, want 10", cfg.WebRTCRecoveryAttempts)
	}
}

// offset은 부호 있는 값이라 튜닝으로 오디오를 어느 쪽으로든 밀 수 있다.
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

func TestValidateEgressVideoSize(t *testing.T) {
	for _, valid := range []string{"", "1280x720", "1920x1080", "640x360"} {
		cfg := validConfig()
		cfg.EgressVideoSize = valid
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() with EgressVideoSize=%q error = %v", valid, err)
		}
	}
	// 홀수 해상도는 libx264의 yuv420p가 받지 못하므로 기동 시점에 거른다.
	for _, invalid := range []string{"1280", "1280x", "x720", "0x720", "-1280x720", "1281x721", "hdx720", "99999x720"} {
		cfg := validConfig()
		cfg.EgressVideoSize = invalid
		if err := cfg.Validate(); err == nil {
			t.Errorf("expected EgressVideoSize=%q to be rejected", invalid)
		}
	}
}

// TestValidateDecoderPinLongEdge는 디코더 핀의 허용 범위를 고정한다. 상한
// 1920은 AI 서버의 MAX_LONG_EDGE와 같아서, 넘기면 프레임이 도착하자마자
// 리사이즈 없이 거부돼 세션이 통째로 블랙아웃된다(#122).
func TestValidateDecoderPinLongEdge(t *testing.T) {
	// 0은 미설정 = 기존 동작 유지다.
	for _, valid := range []int{0, 32, 640, 1280, 1920} {
		cfg := validConfig()
		cfg.DecoderPinLongEdge = valid
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() with DecoderPinLongEdge=%d error = %v", valid, err)
		}
	}
	// 홀수(yuv420p), 하한 미만(AI가 거부), 상한 초과(AI가 거부), 음수.
	for _, invalid := range []int{1, 30, 31, 1281, 1922, 3840, -1280} {
		cfg := validConfig()
		cfg.DecoderPinLongEdge = invalid
		if err := cfg.Validate(); err == nil {
			t.Errorf("expected DecoderPinLongEdge=%d to be rejected", invalid)
		}
	}
}

// TestValidateAIPreflightInterval: 0은 상시 감시 비활성이므로 통과해야 하고,
// 주기가 타임아웃보다 짧거나 같으면 프로브가 워커에 쌓이므로 거부해야 한다(#162).
func TestValidateAIPreflightInterval(t *testing.T) {
	for _, valid := range []time.Duration{0, 31 * time.Second, 5 * time.Minute} {
		cfg := validConfig()
		cfg.AIPreflightTimeout = 30 * time.Second
		cfg.AIPreflightInterval = valid
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() with AIPreflightInterval=%s error = %v", valid, err)
		}
	}
	for _, invalid := range []time.Duration{-time.Second, 10 * time.Second, 30 * time.Second} {
		cfg := validConfig()
		cfg.AIPreflightTimeout = 30 * time.Second
		cfg.AIPreflightInterval = invalid
		if err := cfg.Validate(); err == nil {
			t.Errorf("expected AIPreflightInterval=%s to be rejected", invalid)
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
