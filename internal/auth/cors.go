package auth

import (
	"inno-live-server/internal/origin"
)

// TokenHTTPConfig is kept as an alias for compatibility. The policy itself is
// shared by token, application HTTP, and WebSocket handlers.
type TokenHTTPConfig = origin.Config

func LoadTokenHTTPConfigFromEnv() (TokenHTTPConfig, error) {
	return origin.LoadFromEnv()
}

func NewTokenHTTPConfig(allowAll bool, origins []string) (TokenHTTPConfig, error) {
	return origin.NewConfig(allowAll, origins)
}
