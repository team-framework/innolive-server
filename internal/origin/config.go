// Package origin은 HTTP와 WebSocket 엔드포인트가 함께 쓰는 Origin 정책을 제공한다.
// Origin 헤더가 없는 요청은 일부러 브라우저가 아닌 요청으로 본다 — CORS는
// 브라우저에서 온 요청만 다룬다.
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

// Config는 어떤 브라우저 origin이 애플리케이션에 접근할 수 있는지 정한다.
type Config struct {
	AllowAllOrigins bool
	AllowedOrigins  []string
	allowedOrigins  map[string]struct{}
}

// LoadFromEnv는 애플리케이션 전체 CORS 정책을 읽는다. 환경 변수 이름은 기존 배포와
// 호환되도록 AUTH_ 접두를 유지한다.
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

// AllowedOrigin은 비어 있지 않은 요청 Origin에 대한 Access-Control-Allow-Origin
// 응답 값이다. 전체 허용은 일부러 "*"를 돌려줘 임의 사이트에 자격 정보를 켜지
// 않는다.
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

// Allows는 요청 origin이 연결할 수 있는지다. 브라우저가 아닌 클라이언트는 Origin을
// 보내지 않으며 같은 정책에서 계속 지원된다.
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
