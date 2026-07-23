package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
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
	YoutubeStreamKey        string
	RequireSessionAuth      bool
	LogLevel                string
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
		YoutubeStreamKey:        strings.TrimSpace(os.Getenv("YOUTUBE_STREAM_KEY")),
		RequireSessionAuth:      envBool("INNOLIVE_REQUIRE_SESSION_AUTH", true),
		UDPMuxPort:              envInt("WEBRTC_UDP_MUX_PORT", 0),
		LogLevel:                strings.ToUpper(env("LOG_LEVEL", "INFO")),
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
	case "", WireFormatJPEG, WireFormatRaw: // empty defaults to jpeg downstream
	default:
		return fmt.Errorf("AI_FRAME_WIRE_FORMAT must be one of jpeg, raw: %q", c.AIWireFormat)
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
