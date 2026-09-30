package auth

import (
	"inno-live-server/internal/origin"
)

// TokenHTTPConfig는 호환을 위해 남긴 별칭이다. 정책 자체는 토큰, 애플리케이션 HTTP,
// WebSocket 핸들러가 함께 쓴다.
type TokenHTTPConfig = origin.Config

func LoadTokenHTTPConfigFromEnv() (TokenHTTPConfig, error) {
	return origin.LoadFromEnv()
}

func NewTokenHTTPConfig(allowAll bool, origins []string) (TokenHTTPConfig, error) {
	return origin.NewConfig(allowAll, origins)
}
