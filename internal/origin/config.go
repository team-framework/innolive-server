// Package origin provides the single Origin policy used by HTTP and WebSocket
// endpoints. It deliberately treats requests without an Origin header as
// non-browser requests, because CORS only governs browser-originated requests.
package origin

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Config controls which browser origins may access the application.
type Config struct {
	AllowAllOrigins bool
	AllowedOrigins  []string
	allowedOrigins  map[string]struct{}
}

// LoadFromEnv loads the application-wide CORS policy. The environment names
// retain their AUTH_ prefix for backward compatibility with existing deploys.
func LoadFromEnv() (Config, error) {
	allowAll, err := envBool("AUTH_CORS_ALLOW_ALL_ORIGINS", false)
	if err != nil {
		return Config{}, err
	}

	var origins []string
	for _, raw := range strings.Split(os.Getenv("AUTH_CORS_ALLOWED_ORIGINS"), ",") {
		if value := strings.TrimSpace(raw); value != "" {
			origins = append(origins, value)
		}
	}
	return NewConfig(allowAll, origins)
}

func NewConfig(allowAll bool, origins []string) (Config, error) {
	allowed := make(map[string]struct{}, len(origins))
	for _, raw := range origins {
		normalized, err := normalize(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid AUTH_CORS_ALLOWED_ORIGINS entry %q: %w", raw, err)
		}
		allowed[normalized] = struct{}{}
	}

	normalized := make([]string, 0, len(allowed))
	for value := range allowed {
		normalized = append(normalized, value)
	}
	sort.Strings(normalized)

	return Config{AllowAllOrigins: allowAll, AllowedOrigins: normalized, allowedOrigins: allowed}, nil
}

// AllowedOrigin returns the response Access-Control-Allow-Origin value for a
// non-empty request Origin. Allow-all intentionally returns "*" so credentials
// are not enabled for arbitrary sites.
func (c Config) AllowedOrigin(requestOrigin string) (string, bool) {
	if c.AllowAllOrigins {
		return "*", true
	}
	normalized, err := normalize(requestOrigin)
	if err != nil {
		return "", false
	}
	_, ok := c.allowedOrigins[normalized]
	return normalized, ok
}

// Allows reports whether a request origin may connect. Non-browser clients do
// not send Origin and remain supported by the same policy.
func (c Config) Allows(requestOrigin string) bool {
	if strings.TrimSpace(requestOrigin) == "" {
		return true
	}
	_, ok := c.AllowedOrigin(requestOrigin)
	return ok
}

func envBool(key string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return parsed, nil
}

func normalize(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("origin must not be empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("origin scheme must be http or https")
	}
	if parsed.Host == "" || parsed.User != nil {
		return "", errors.New("origin must contain only scheme and host")
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("origin must not contain a path, query, or fragment")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}
