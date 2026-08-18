package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

type DatabaseMigrationMode string

const (
	DatabaseMigrationModeAuto      DatabaseMigrationMode = "auto"
	DatabaseMigrationModeVersioned DatabaseMigrationMode = "versioned"
	DatabaseMigrationModeOff       DatabaseMigrationMode = "off"
)

type PrivacyMode string

const (
	PrivacyModeBypass     PrivacyMode = "bypass"
	PrivacyModeFixedDelay PrivacyMode = "fixed_delay"
	PrivacyModeReal       PrivacyMode = "real"
)

type WireFormat string

const (
	WireFormatJPEG WireFormat = "jpeg"
	WireFormatRaw  WireFormat = "raw"
)

type AIFailurePolicy string

const (
	FailurePolicyBlackoutLatch AIFailurePolicy = "blackout_latch"
	FailurePolicyFreeze        AIFailurePolicy = "freeze"
)

type Config struct {
	HTTPAddr                string
	PrivacyMode             PrivacyMode
	PrivacyFixedDelay       time.Duration
	AIAddress               string
	AITargets               []string
	AITimeout               time.Duration
	AIInsecure              bool
	AIWireFormat            WireFormat
	AIFailurePolicy         AIFailurePolicy
	AITimeoutLatchThreshold int
	AIPreflightTimeout      time.Duration
	AIPreflightRequired     bool
	AIMeImagePath           string
	ReferenceStorePath      string
	FFmpegPath              string
	FFmpegSpawnConcurrency  int
	FFmpegEncoderThreads    int
	MaxSessions             int
	NegotiationTimeout      time.Duration
	STUNURLs                []string
	TURNURLs                []string
	TURNUsername            string
	TURNCredential          string
	AnnouncedIP             string
	UDPPortMin              uint16
	UDPPortMax              uint16
	UDPMuxPort              int
	DisconnectedGracePeriod time.Duration
	FrameQueueSize          int
	EnableAudioEgress       bool
	EgressLatencyLog        bool
	EgressAudioOffset       time.Duration
	EgressVideoBitrate      string
	EgressVideoSize         string
	RequireSessionAuth      bool
	LogLevel                string
	DatabaseURL             string
	DatabaseMaxOpenConns    int
	DatabaseMaxIdleConns    int
	DatabaseConnMaxLifetime time.Duration
	DatabaseConnMaxIdleTime time.Duration
	DatabaseMigrationMode   DatabaseMigrationMode
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:                env("HTTP_ADDR", ":8000"),
		PrivacyMode:             PrivacyMode(env("AI_PRIVACY_MODE", string(PrivacyModeReal))),
		AIAddress:               env("AI_GRPC_ADDR", "localhost:50051"),
		AIInsecure:              envBool("AI_GRPC_INSECURE", true),
		AIWireFormat:            WireFormat(env("AI_FRAME_WIRE_FORMAT", string(WireFormatJPEG))),
		AIFailurePolicy:         AIFailurePolicy(env("AI_FAILURE_POLICY", string(FailurePolicyBlackoutLatch))),
		AITimeoutLatchThreshold: envInt("AI_TIMEOUT_LATCH_THRESHOLD", 3),
		AIPreflightTimeout:      envDuration("AI_PREFLIGHT_TIMEOUT", 30*time.Second),
		AIPreflightRequired:     envBool("AI_PREFLIGHT_REQUIRED", false),
		AIMeImagePath:           strings.TrimSpace(os.Getenv("AI_PRIVACY_ME_IMAGE_PATH")),
		ReferenceStorePath:      strings.TrimSpace(os.Getenv("REFERENCE_STORE_PATH")),
		FFmpegPath:              env("FFMPEG_PATH", "ffmpeg"),
		FFmpegSpawnConcurrency:  envInt("FFMPEG_SPAWN_CONCURRENCY", 3),
		FFmpegEncoderThreads:    envInt("FFMPEG_ENCODER_THREADS", 1),
		MaxSessions:             envInt("MAX_SESSIONS", 0),
		NegotiationTimeout:      envDuration("SESSION_NEGOTIATION_TIMEOUT", 30*time.Second),
		STUNURLs:                splitURLs(env("WEBRTC_STUN_URLS", "stun:stun.l.google.com:19302")),
		TURNURLs:                splitURLs(env("WEBRTC_TURN_URLS", "")),
		TURNUsername:            strings.TrimSpace(os.Getenv("WEBRTC_TURN_USERNAME")),
		TURNCredential:          strings.TrimSpace(os.Getenv("WEBRTC_TURN_CREDENTIAL")),
		AnnouncedIP:             strings.TrimSpace(os.Getenv("WEBRTC_ANNOUNCED_IP")),
		DisconnectedGracePeriod: envDurationWithSecondsAlias("WEBRTC_DISCONNECTED_GRACE", "WEBRTC_DISCONNECTED_GRACE_SECONDS", 10*time.Second),
		AITimeout:               envDuration("AI_GRPC_TIMEOUT", 5*time.Second),
		PrivacyFixedDelay:       envDurationWithMillisecondsAlias("AI_PRIVACY_FIXED_DELAY", "AI_PRIVACY_FIXED_DELAY_MS", 20*time.Millisecond),
		FrameQueueSize:          envInt("AI_FRAME_QUEUE_SIZE", 2),
		EnableAudioEgress:       envBool("ENABLE_AUDIO_EGRESS", false),
		EgressLatencyLog:        envBool("EGRESS_LATENCY_LOG", false),
		EgressAudioOffset:       time.Duration(envInt("EGRESS_AUDIO_OFFSET_MS", 0)) * time.Millisecond,
		EgressVideoBitrate:      strings.TrimSpace(os.Getenv("EGRESS_VIDEO_BITRATE")),
		EgressVideoSize:         strings.TrimSpace(os.Getenv("EGRESS_VIDEO_SIZE")),
		RequireSessionAuth:      envBool("INNOLIVE_REQUIRE_SESSION_AUTH", true),
		UDPMuxPort:              envInt("WEBRTC_UDP_MUX_PORT", 0),
		LogLevel:                strings.ToUpper(env("LOG_LEVEL", "INFO")),
		DatabaseURL:             strings.TrimSpace(os.Getenv("DATABASE_URL")),
		DatabaseMaxOpenConns:    envInt("DATABASE_MAX_OPEN_CONNS", 10),
		DatabaseMaxIdleConns:    envInt("DATABASE_MAX_IDLE_CONNS", 5),
		DatabaseConnMaxLifetime: envDuration("DATABASE_CONN_MAX_LIFETIME", 30*time.Minute),
		DatabaseConnMaxIdleTime: envDuration("DATABASE_CONN_MAX_IDLE_TIME", 5*time.Minute),
		DatabaseMigrationMode: DatabaseMigrationMode(
			env("DATABASE_MIGRATION_MODE", string(DatabaseMigrationModeAuto)),
		),
	}

	minPort, err := envUint16("WEBRTC_UDP_PORT_MIN", 50000)
	if err != nil {
		return Config{}, err
	}
	maxPort, err := envUint16("WEBRTC_UDP_PORT_MAX", 60000)
	if err != nil {
		return Config{}, err
	}
	cfg.UDPPortMin = minPort
	cfg.UDPPortMax = maxPort

	cfg.AITargets = splitList(env("AI_GRPC_TARGETS", ""))
	if len(cfg.AITargets) == 0 {
		cfg.AITargets = []string{cfg.AIAddress}
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	switch c.PrivacyMode {
	case PrivacyModeBypass, PrivacyModeFixedDelay, PrivacyModeReal:
	default:
		return fmt.Errorf("AI_PRIVACY_MODE must be one of bypass, fixed_delay, real: %q", c.PrivacyMode)
	}
	if c.HTTPAddr == "" {
		return errors.New("HTTP_ADDR must not be empty")
	}
	if c.PrivacyMode == PrivacyModeReal && c.AIAddress == "" {
		return errors.New("AI_GRPC_ADDR must not be empty in real mode")
	}
	if c.FFmpegPath == "" {
		return errors.New("FFMPEG_PATH must not be empty")
	}
	if c.PrivacyFixedDelay < 0 {
		return errors.New("AI_PRIVACY_FIXED_DELAY must not be negative")
	}
	if c.AITimeout <= 0 {
		return errors.New("AI_GRPC_TIMEOUT must be positive")
	}
	if c.UDPPortMin == 0 || c.UDPPortMax == 0 || c.UDPPortMin > c.UDPPortMax {
		return errors.New("WEBRTC UDP port range is invalid")
	}
	if c.UDPMuxPort < 0 || c.UDPMuxPort > 65535 {
		return errors.New("WEBRTC_UDP_MUX_PORT must be between 0 and 65535 (0 disables the mux)")
	}
	if c.FrameQueueSize < 1 {
		return errors.New("AI_FRAME_QUEUE_SIZE must be at least 1")
	}
	switch c.AIWireFormat {
	case "", WireFormatJPEG: // empty defaults to jpeg downstream
	case WireFormatRaw:
		return errors.New("AI_FRAME_WIRE_FORMAT=raw is not supported by this AI server's proto (VideoChunk has no width/height/pix_fmt fields to carry raw yuv420p) — use jpeg")
	default:
		return fmt.Errorf("AI_FRAME_WIRE_FORMAT must be jpeg: %q", c.AIWireFormat)
	}
	switch c.AIFailurePolicy {
	case "", FailurePolicyBlackoutLatch, FailurePolicyFreeze: // empty defaults to blackout_latch downstream
	default:
		return fmt.Errorf("AI_FAILURE_POLICY must be one of blackout_latch, freeze: %q", c.AIFailurePolicy)
	}
	if c.MaxSessions < 0 {
		return errors.New("MAX_SESSIONS must not be negative")
	}
	if c.FFmpegSpawnConcurrency < 0 {
		return errors.New("FFMPEG_SPAWN_CONCURRENCY must not be negative")
	}
	if c.FFmpegEncoderThreads < 0 {
		return errors.New("FFMPEG_ENCODER_THREADS must not be negative")
	}
	if c.AITimeoutLatchThreshold < 0 {
		return errors.New("AI_TIMEOUT_LATCH_THRESHOLD must not be negative")
	}
	if v := c.EgressVideoBitrate; v != "" {
		digits := strings.TrimRight(v, "kKmM")
		if n, err := strconv.Atoi(digits); err != nil || n <= 0 || len(v)-len(digits) > 1 {
			return fmt.Errorf("EGRESS_VIDEO_BITRATE must be a positive number with an optional k/M suffix (e.g. 5000k): %q", v)
		}
	}
	if v := c.EgressVideoSize; v != "" {
		rawWidth, rawHeight, found := strings.Cut(v, "x")
		width, widthErr := strconv.Atoi(rawWidth)
		height, heightErr := strconv.Atoi(rawHeight)
		// libx264의 yuv420p는 짝수 해상도만 받는다.
		if !found || widthErr != nil || heightErr != nil ||
			width <= 0 || height <= 0 || width > math.MaxUint16 || height > math.MaxUint16 ||
			width%2 != 0 || height%2 != 0 {
			return fmt.Errorf("EGRESS_VIDEO_SIZE must be an even WIDTHxHEIGHT (e.g. 1280x720): %q", v)
		}
	}
	if c.AIPreflightTimeout <= 0 {
		return errors.New("AI_PREFLIGHT_TIMEOUT must be positive")
	}
	if c.NegotiationTimeout < 0 {
		return errors.New("SESSION_NEGOTIATION_TIMEOUT must not be negative")
	}
	if c.PrivacyMode == PrivacyModeReal {
		for _, target := range c.AITargets {
			if strings.TrimSpace(target) == "" {
				return errors.New("AI_GRPC_TARGETS must not contain empty addresses")
			}
		}
	}
	if c.DatabaseURL == "" {
		return errors.New("DATABASE_URL must not be empty")
	}
	if c.DatabaseMaxOpenConns < 1 {
		return errors.New("DATABASE_MAX_OPEN_CONNS must be at least 1")
	}
	if c.DatabaseMaxIdleConns < 0 {
		return errors.New("DATABASE_MAX_IDLE_CONNS must not be negative")
	}
	if c.DatabaseMaxIdleConns > c.DatabaseMaxOpenConns {
		return errors.New("DATABASE_MAX_IDLE_CONNS must not exceed DATABASE_MAX_OPEN_CONNS")
	}
	if c.DatabaseConnMaxLifetime <= 0 {
		return errors.New("DATABASE_CONN_MAX_LIFETIME must be positive")
	}
	if c.DatabaseConnMaxIdleTime <= 0 {
		return errors.New("DATABASE_CONN_MAX_IDLE_TIME must be positive")
	}
	switch c.DatabaseMigrationMode {
	case DatabaseMigrationModeAuto,
		DatabaseMigrationModeVersioned,
		DatabaseMigrationModeOff:
		// valid
	default:
		return fmt.Errorf(
			"DATABASE_MIGRATION_MODE must be one of auto, versioned, off: %q",
			c.DatabaseMigrationMode,
		)
	}
	return nil
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envUint16(key string, fallback uint16) (uint16, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 16)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("%s must be a port between 1 and 65535", key)
	}
	return uint16(parsed), nil
}

func splitURLs(value string) []string {
	var urls []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			if strings.EqualFold(item, "none") || strings.EqualFold(item, "off") {
				continue
			}
			urls = append(urls, item)
		}
	}
	return urls
}

func splitList(value string) []string {
	var items []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

func envDurationWithMillisecondsAlias(durationKey, millisecondsKey string, fallback time.Duration) time.Duration {
	if value := strings.TrimSpace(os.Getenv(durationKey)); value != "" {
		parsed, err := time.ParseDuration(value)
		if err == nil {
			return parsed
		}
		return fallback
	}
	if value := strings.TrimSpace(os.Getenv(millisecondsKey)); value != "" {
		milliseconds, err := strconv.ParseFloat(value, 64)
		if err == nil {
			return time.Duration(milliseconds * float64(time.Millisecond))
		}
	}
	return fallback
}

func envDurationWithSecondsAlias(durationKey, secondsKey string, fallback time.Duration) time.Duration {
	if value := strings.TrimSpace(os.Getenv(durationKey)); value != "" {
		parsed, err := time.ParseDuration(value)
		if err == nil {
			return parsed
		}
		return fallback
	}
	if value := strings.TrimSpace(os.Getenv(secondsKey)); value != "" {
		seconds, err := strconv.ParseFloat(value, 64)
		if err == nil {
			return time.Duration(seconds * float64(time.Second))
		}
	}
	return fallback
}
