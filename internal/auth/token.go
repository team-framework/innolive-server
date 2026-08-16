package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const refreshTokenBytes = 32

var (
	ErrInvalidAccessToken  = errors.New("invalid access token")
	ErrInvalidRefreshToken = errors.New("invalid refresh token")
	ErrRefreshTokenExpired = errors.New("refresh token expired")
	ErrRefreshTokenRevoked = errors.New("refresh token revoked")
	ErrRefreshTokenReused  = errors.New("refresh token reuse detected")
	ErrUserInactive        = errors.New("user is not active")
)

type TokenConfig struct {
	AccessKey          []byte
	AccessTTL          time.Duration
	RefreshTTL         time.Duration
	RefreshAbsoluteTTL time.Duration
	Issuer             string
	Audience           string
	ClockSkew          time.Duration
}

func LoadTokenConfigFromEnv() (TokenConfig, error) {
	key, err := decodeAccessKey(strings.TrimSpace(os.Getenv("AUTH_ACCESS_TOKEN_KEY_BASE64")))
	if err != nil {
		return TokenConfig{}, err
	}
	accessTTL, err := tokenEnvDuration("AUTH_ACCESS_TOKEN_TTL", 15*time.Minute)
	if err != nil {
		return TokenConfig{}, err
	}
	refreshTTL, err := tokenEnvDuration("AUTH_REFRESH_TOKEN_TTL", 30*24*time.Hour)
	if err != nil {
		return TokenConfig{}, err
	}
	refreshAbsoluteTTL, err := tokenEnvDuration("AUTH_REFRESH_TOKEN_ABSOLUTE_TTL", 90*24*time.Hour)
	if err != nil {
		return TokenConfig{}, err
	}
	clockSkew, err := tokenEnvDuration("AUTH_TOKEN_CLOCK_SKEW", 30*time.Second)
	if err != nil {
		return TokenConfig{}, err
	}
	cfg := TokenConfig{
		AccessKey:          key,
		AccessTTL:          accessTTL,
		RefreshTTL:         refreshTTL,
		RefreshAbsoluteTTL: refreshAbsoluteTTL,
		Issuer:             tokenEnv("AUTH_TOKEN_ISSUER", "innolive-server"),
		Audience:           tokenEnv("AUTH_TOKEN_AUDIENCE", "innolive-api"),
		ClockSkew:          clockSkew,
	}
	if err := cfg.Validate(); err != nil {
		return TokenConfig{}, err
	}
	return cfg, nil
}

func (c TokenConfig) Validate() error {
	if len(c.AccessKey) < 32 {
		return errors.New("AUTH_ACCESS_TOKEN_KEY_BASE64 must decode to at least 32 bytes")
	}
	if c.AccessTTL <= 0 {
		return errors.New("AUTH_ACCESS_TOKEN_TTL must be positive")
	}
	if c.RefreshTTL <= 0 {
		return errors.New("AUTH_REFRESH_TOKEN_TTL must be positive")
	}
	if c.RefreshAbsoluteTTL <= 0 {
		return errors.New("AUTH_REFRESH_TOKEN_ABSOLUTE_TTL must be positive")
	}
	if c.RefreshAbsoluteTTL < c.RefreshTTL {
		return errors.New("AUTH_REFRESH_TOKEN_ABSOLUTE_TTL must be greater than or equal to AUTH_REFRESH_TOKEN_TTL")
	}
	if c.ClockSkew < 0 {
		return errors.New("AUTH_TOKEN_CLOCK_SKEW must not be negative")
	}
	if strings.TrimSpace(c.Issuer) == "" {
		return errors.New("AUTH_TOKEN_ISSUER must not be empty")
	}
	if strings.TrimSpace(c.Audience) == "" {
		return errors.New("AUTH_TOKEN_AUDIENCE must not be empty")
	}
	return nil
}

func decodeAccessKey(value string) ([]byte, error) {
	if value == "" {
		return nil, errors.New("AUTH_ACCESS_TOKEN_KEY_BASE64 is required")
	}
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, encoding := range encodings {
		decoded, err := encoding.DecodeString(value)
		if err == nil {
			if len(decoded) < 32 {
				return nil, errors.New("AUTH_ACCESS_TOKEN_KEY_BASE64 must decode to at least 32 bytes")
			}
			return decoded, nil
		}
	}
	return nil, errors.New("AUTH_ACCESS_TOKEN_KEY_BASE64 is not valid base64")
}

func tokenEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func tokenEnvDuration(key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration: %w", key, err)
	}
	return parsed, nil
}

type AccessClaims struct {
	SessionID string `json:"sid"`
	TokenType string `json:"token_type"`
	jwt.RegisteredClaims
}

type ClientInfo struct {
	UserAgent string
	IPAddress string
}

type TokenPair struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshExpiresIn int64  `json:"refresh_expires_in"`
}

type refreshSessionRecord struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	FamilyID     uuid.UUID
	TokenHash    []byte
	UserAgent    *string
	IPAddress    *string
	ExpiresAt    time.Time
	LastUsedAt   *time.Time
	RevokedAt    *time.Time
	RevokeReason *string
	ReplacedByID *uuid.UUID
	CreatedAt    time.Time
}

type refreshRotation struct {
	ID            uuid.UUID
	TokenHash     []byte
	IdleExpiresAt time.Time
	AbsoluteTTL   time.Duration
	Client        ClientInfo
}

type refreshStore interface {
	Create(context.Context, refreshSessionRecord) error
	Rotate(context.Context, []byte, time.Time, refreshRotation) (refreshSessionRecord, error)
	RevokeFamilyByHash(context.Context, []byte, time.Time) (uuid.UUID, error)
}

type TokenService struct {
	store refreshStore
	cfg   TokenConfig
	now   func() time.Time
}

func NewTokenService(db *gorm.DB, cfg TokenConfig) *TokenService {
	return newTokenService(&gormRefreshStore{db: db}, cfg)
}

func newTokenService(store refreshStore, cfg TokenConfig) *TokenService {
	return &TokenService{store: store, cfg: cfg, now: func() time.Time { return time.Now().UTC() }}
}

func (s *TokenService) IssuePair(ctx context.Context, userID uuid.UUID, client ClientInfo) (TokenPair, error) {
	now := s.now().UTC()
	sessionID := uuid.New()
	familyID := uuid.New()
	rawRefresh, refreshHash, err := newRefreshToken()
	if err != nil {
		return TokenPair{}, fmt.Errorf("generate refresh token: %w", err)
	}
	access, err := s.issueAccessToken(userID, sessionID, now)
	if err != nil {
		return TokenPair{}, err
	}
	refreshExpiresAt := earlierTime(
		now.Add(s.cfg.RefreshTTL),
		now.Add(s.cfg.RefreshAbsoluteTTL),
	)
	record := refreshSessionRecord{
		ID:        sessionID,
		UserID:    userID,
		FamilyID:  familyID,
		TokenHash: refreshHash,
		UserAgent: tokenOptionalString(client.UserAgent),
		IPAddress: tokenOptionalString(client.IPAddress),
		ExpiresAt: refreshExpiresAt,
		CreatedAt: now,
	}
	if err := s.store.Create(ctx, record); err != nil {
		return TokenPair{}, fmt.Errorf("store refresh session: %w", err)
	}
	return s.pair(access, rawRefresh, refreshExpiresAt, now), nil
}

func (s *TokenService) Rotate(ctx context.Context, rawRefresh string, client ClientInfo) (TokenPair, error) {
	oldHash, err := hashRefreshToken(rawRefresh)
	if err != nil {
		return TokenPair{}, ErrInvalidRefreshToken
	}
	now := s.now().UTC()
	newID := uuid.New()
	rawNext, nextHash, err := newRefreshToken()
	if err != nil {
		return TokenPair{}, fmt.Errorf("generate refresh token: %w", err)
	}
	next, err := s.store.Rotate(ctx, oldHash, now, refreshRotation{
		ID:            newID,
		TokenHash:     nextHash,
		IdleExpiresAt: now.Add(s.cfg.RefreshTTL),
		AbsoluteTTL:   s.cfg.RefreshAbsoluteTTL,
		Client:        client,
	})
	if err != nil {
		return TokenPair{}, err
	}
	access, err := s.issueAccessToken(next.UserID, newID, now)
	if err != nil {
		return TokenPair{}, err
	}
	return s.pair(access, rawNext, next.ExpiresAt, now), nil
}

func (s *TokenService) Logout(ctx context.Context, rawRefresh string) error {
	_, err := s.LogoutUser(ctx, rawRefresh)
	return err
}

// LogoutUser는 refresh token family를 폐기하고, 폐기된 family의 사용자를
// 반환한다. HTTP 계층은 성공한 로그아웃 뒤 이 ID로 활성 WebRTC/RTMP 세션을
// 닫는다. 토큰이 유효하지 않으면 사용자 ID를 반환하지 않는다.
func (s *TokenService) LogoutUser(ctx context.Context, rawRefresh string) (uuid.UUID, error) {
	hash, err := hashRefreshToken(rawRefresh)
	if err != nil {
		return uuid.Nil, ErrInvalidRefreshToken
	}
	return s.store.RevokeFamilyByHash(ctx, hash, s.now().UTC())
}

func (s *TokenService) ValidateAccessToken(raw string) (*AccessClaims, error) {
	claims := &AccessClaims{}
	token, err := jwt.ParseWithClaims(
		raw,
		claims,
		func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, fmt.Errorf("unexpected signing method: %s", token.Method.Alg())
			}
			return s.cfg.AccessKey, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(s.cfg.Issuer),
		jwt.WithAudience(s.cfg.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(s.cfg.ClockSkew),
	)
	if err != nil || !token.Valid || claims.TokenType != "access" {
		return nil, ErrInvalidAccessToken
	}
	if claims.ExpiresAt == nil || claims.NotBefore == nil || claims.IssuedAt == nil {
		return nil, ErrInvalidAccessToken
	}
	if _, err := uuid.Parse(claims.Subject); err != nil {
		return nil, ErrInvalidAccessToken
	}
	if _, err := uuid.Parse(claims.SessionID); err != nil {
		return nil, ErrInvalidAccessToken
	}
	if _, err := uuid.Parse(claims.ID); err != nil {
		return nil, ErrInvalidAccessToken
	}
	return claims, nil
}

func (s *TokenService) issueAccessToken(userID, sessionID uuid.UUID, now time.Time) (string, error) {
	claims := AccessClaims{
		SessionID: sessionID.String(),
		TokenType: "access",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.cfg.Issuer,
			Subject:   userID.String(),
			Audience:  jwt.ClaimStrings{s.cfg.Audience},
			ExpiresAt: jwt.NewNumericDate(now.Add(s.cfg.AccessTTL)),
			NotBefore: jwt.NewNumericDate(now),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        uuid.NewString(),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(s.cfg.AccessKey)
	if err != nil {
		return "", fmt.Errorf("sign access token: %w", err)
	}
	return signed, nil
}

func (s *TokenService) pair(access, refresh string, refreshExpiresAt, now time.Time) TokenPair {
	return TokenPair{
		AccessToken:      access,
		RefreshToken:     refresh,
		TokenType:        "Bearer",
		ExpiresIn:        int64(s.cfg.AccessTTL / time.Second),
		RefreshExpiresIn: remainingSeconds(refreshExpiresAt, now),
	}
}

func remainingSeconds(expiresAt, now time.Time) int64 {
	remaining := expiresAt.Sub(now)
	if remaining <= 0 {
		return 0
	}
	return int64(remaining / time.Second)
}

func earlierTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

func newRefreshToken() (string, []byte, error) {
	value := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(value); err != nil {
		return "", nil, err
	}
	raw := base64.RawURLEncoding.EncodeToString(value)
	hash := sha256.Sum256(value)
	return raw, hash[:], nil
}

func hashRefreshToken(raw string) ([]byte, error) {
	value, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil || len(value) != refreshTokenBytes {
		return nil, ErrInvalidRefreshToken
	}
	hash := sha256.Sum256(value)
	return hash[:], nil
}

func tokenOptionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
