package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	appleIssuer    = "https://appleid.apple.com"
	appleTokenURL  = appleIssuer + "/auth/token"
	appleKeysURL   = appleIssuer + "/auth/keys"
	appleRevokeURL = appleIssuer + "/auth/revoke"
	appleSecretTTL = 180 * 24 * time.Hour
	maxAppleField  = 512
)

var ErrInvalidAppleIDToken = errors.New("invalid Apple ID token")

type AppleOAuthConfig struct {
	TeamID         string
	ClientID       string
	KeyID          string
	PrivateKeyPath string
}

func LoadAppleOAuthConfigFromEnv() (AppleOAuthConfig, error) {
	config := AppleOAuthConfig{
		TeamID:         strings.TrimSpace(os.Getenv("APPLE_TEAM_ID")),
		ClientID:       strings.TrimSpace(os.Getenv("APPLE_CLIENT_ID")),
		KeyID:          strings.TrimSpace(os.Getenv("APPLE_KEY_ID")),
		PrivateKeyPath: strings.TrimSpace(os.Getenv("APPLE_PRIVATE_KEY_PATH")),
	}
	values := []string{config.TeamID, config.ClientID, config.KeyID, config.PrivateKeyPath}
	empty := 0
	for _, value := range values {
		if value == "" {
			empty++
		}
		if utf8.RuneCountInString(value) > maxAppleField {
			return AppleOAuthConfig{}, errors.New("Apple OAuth configuration value is too long")
		}
	}
	if empty == len(values) {
		return AppleOAuthConfig{}, nil
	}
	if empty != 0 {
		return AppleOAuthConfig{}, errors.New("APPLE_TEAM_ID, APPLE_CLIENT_ID, APPLE_KEY_ID, and APPLE_PRIVATE_KEY_PATH must be configured together")
	}
	return config, nil
}

func (c AppleOAuthConfig) Enabled() bool { return c.ClientID != "" }

func (c AppleOAuthConfig) SupportsClientID(clientID string) bool {
	clientID = strings.TrimSpace(clientID)
	return clientID != "" && clientID == c.ClientID
}

type AppleTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
}

type AppleAuthorizationExchanger interface {
	Exchange(context.Context, string, string) (AppleTokenResponse, error)
}

type AppleIdentityVerifier interface {
	Verify(context.Context, string, string, string) (AppleIdentity, error)
}

type AppleTokenRevoker interface {
	Revoke(context.Context, string) error
}

type appleOAuthClient struct {
	config     AppleOAuthConfig
	privateKey *ecdsa.PrivateKey
	httpClient *http.Client
	tokenURL   string
	keysURL    string
	revokeURL  string
	keysMu     sync.Mutex
	keys       map[string]*rsa.PublicKey
	keysUntil  time.Time
	now        func() time.Time
}

func NewAppleOAuthClient(config AppleOAuthConfig) (*appleOAuthClient, error) {
	if !config.Enabled() {
		return nil, errors.New("Apple OAuth is not configured")
	}
	data, err := os.ReadFile(config.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read APPLE_PRIVATE_KEY_PATH: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("APPLE_PRIVATE_KEY_PATH does not contain a PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse Apple private key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("Apple private key must use the P-256 curve")
	}
	return &appleOAuthClient{
		config: config, privateKey: key,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		tokenURL:   appleTokenURL, keysURL: appleKeysURL, revokeURL: appleRevokeURL,
		keys: make(map[string]*rsa.PublicKey), now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (c *appleOAuthClient) Exchange(ctx context.Context, authorizationCode, clientID string) (AppleTokenResponse, error) {
	if !c.config.SupportsClientID(clientID) {
		return AppleTokenResponse{}, ErrInvalidAppleIDToken
	}
	secret, err := c.clientSecret(clientID)
	if err != nil {
		return AppleTokenResponse{}, err
	}
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {secret},
		"code":          {strings.TrimSpace(authorizationCode)},
		"grant_type":    {"authorization_code"},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return AppleTokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return AppleTokenResponse{}, fmt.Errorf("request Apple token exchange: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
		return AppleTokenResponse{}, fmt.Errorf("Apple token exchange returned HTTP %d", response.StatusCode)
	}
	var result AppleTokenResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 32<<10)).Decode(&result); err != nil {
		return AppleTokenResponse{}, fmt.Errorf("decode Apple token response: %w", err)
	}
	if strings.TrimSpace(result.IDToken) == "" {
		return AppleTokenResponse{}, errors.New("Apple token response has no id_token")
	}
	return result, nil
}

func (c *appleOAuthClient) Revoke(ctx context.Context, refreshToken string) error {
	secret, err := c.clientSecret(c.config.ClientID)
	if err != nil {
		return err
	}
	form := url.Values{
		"client_id":       {c.config.ClientID},
		"client_secret":   {secret},
		"token":           {strings.TrimSpace(refreshToken)},
		"token_type_hint": {"refresh_token"},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.revokeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("request Apple token revocation: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
		return fmt.Errorf("Apple token revocation returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (c *appleOAuthClient) clientSecret(clientID string) (string, error) {
	if !c.config.SupportsClientID(clientID) {
		return "", ErrInvalidAppleIDToken
	}
	now := c.now().UTC()
	claims := jwt.RegisteredClaims{
		Issuer: c.config.TeamID, Subject: clientID,
		Audience: jwt.ClaimStrings{appleIssuer},
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(appleSecretTTL)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = c.config.KeyID
	return token.SignedString(c.privateKey)
}

type appleIDTokenClaims struct {
	Email          string `json:"email"`
	EmailVerified  any    `json:"email_verified"`
	IsPrivateEmail any    `json:"is_private_email"`
	Nonce          string `json:"nonce"`
	jwt.RegisteredClaims
}

type AppleIdentity struct {
	Subject        string
	Email          string
	EmailVerified  bool
	IsPrivateEmail bool
	DisplayName    string
}

func (c *appleOAuthClient) Verify(ctx context.Context, rawToken, expectedNonce, clientID string) (AppleIdentity, error) {
	if !c.config.SupportsClientID(clientID) {
		return AppleIdentity{}, ErrInvalidAppleIDToken
	}
	claims := &appleIDTokenClaims{}
	parsed, err := jwt.ParseWithClaims(strings.TrimSpace(rawToken), claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodRS256 {
			return nil, ErrInvalidAppleIDToken
		}
		kid, _ := token.Header["kid"].(string)
		return c.keyFor(ctx, kid)
	}, jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}), jwt.WithIssuer(appleIssuer), jwt.WithAudience(clientID), jwt.WithExpirationRequired())
	if err != nil || parsed == nil || !parsed.Valid {
		return AppleIdentity{}, ErrInvalidAppleIDToken
	}
	if expectedNonce != "" && claims.Nonce != expectedNonce {
		return AppleIdentity{}, ErrInvalidAppleIDToken
	}
	identity := AppleIdentity{
		Subject: strings.TrimSpace(claims.Subject), Email: appleClaimString(claims.Email),
		EmailVerified: appleClaimBool(claims.EmailVerified), IsPrivateEmail: appleClaimBool(claims.IsPrivateEmail),
	}
	if err := identity.Validate(); err != nil {
		return AppleIdentity{}, err
	}
	return identity, nil
}

func (i AppleIdentity) Validate() error {
	if i.Subject == "" || utf8.RuneCountInString(i.Subject) > 255 || utf8.RuneCountInString(i.Email) > 320 {
		return ErrInvalidAppleIDToken
	}
	return nil
}

func appleClaimString(value string) string { return strings.TrimSpace(strings.ToValidUTF8(value, "")) }

func appleClaimBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

func (c *appleOAuthClient) keyFor(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if strings.TrimSpace(kid) == "" {
		return nil, ErrInvalidAppleIDToken
	}
	c.keysMu.Lock()
	defer c.keysMu.Unlock()
	if c.now().Before(c.keysUntil) {
		if key := c.keys[kid]; key != nil {
			return key, nil
		}
	}
	if err := c.refreshKeys(ctx); err != nil {
		return nil, err
	}
	key := c.keys[kid]
	if key == nil {
		return nil, ErrInvalidAppleIDToken
	}
	return key, nil
}

func (c *appleOAuthClient) refreshKeys(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.keysURL, nil)
	if err != nil {
		return err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("fetch Apple signing keys: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Apple signing keys returned HTTP %d", response.StatusCode)
	}
	var raw struct {
		Keys []appleJWK `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&raw); err != nil {
		return err
	}
	keys := make(map[string]*rsa.PublicKey, len(raw.Keys))
	for _, item := range raw.Keys {
		if key := item.publicKey(); key != nil {
			keys[item.KID] = key
		}
	}
	if len(keys) == 0 {
		return errors.New("Apple signing key set has no RS256 keys")
	}
	c.keys, c.keysUntil = keys, c.now().Add(time.Hour)
	return nil
}

type appleJWK struct {
	KTY string `json:"kty"`
	KID string `json:"kid"`
	ALG string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (j appleJWK) publicKey() *rsa.PublicKey {
	if j.KTY != "RSA" || j.ALG != "RS256" || j.KID == "" {
		return nil
	}
	n, nErr := base64.RawURLEncoding.DecodeString(j.N)
	e, eErr := base64.RawURLEncoding.DecodeString(j.E)
	if nErr != nil || eErr != nil {
		return nil
	}
	modulus := new(big.Int).SetBytes(n)
	exponentValue := new(big.Int).SetBytes(e)
	if modulus.Sign() <= 0 || exponentValue.Sign() <= 0 || !exponentValue.IsInt64() {
		return nil
	}
	exponent64 := exponentValue.Int64()
	exponent := int(exponent64)
	if exponent <= 0 || int64(exponent) != exponent64 {
		return nil
	}
	return &rsa.PublicKey{N: modulus, E: exponent}
}

type appleLoginUser struct {
	ID     uuid.UUID
	Status UserStatus
}

type AppleAccountResolver interface {
	ResolveAppleIdentity(context.Context, AppleIdentity, []byte, *int16) (appleLoginUser, error)
}

type gormAppleAccountResolver struct {
	db  *gorm.DB
	now func() time.Time
}

func NewGormAppleAccountResolver(db *gorm.DB) AppleAccountResolver {
	return &gormAppleAccountResolver{db: db, now: func() time.Time { return time.Now().UTC() }}
}

func (s *gormAppleAccountResolver) ResolveAppleIdentity(ctx context.Context, identity AppleIdentity, ciphertext []byte, version *int16) (appleLoginUser, error) {
	if s == nil || s.db == nil {
		return appleLoginUser{}, errors.New("Apple account database is nil")
	}
	if err := identity.Validate(); err != nil {
		return appleLoginUser{}, err
	}
	var result appleLoginUser
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var account OAuthAccount
		query := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Preload("User").Where("provider = ? AND provider_subject = ?", OAuthProviderApple, identity.Subject).Take(&account)
		if query.Error == nil {
			if account.User == nil {
				return errors.New("Apple OAuth account has no user")
			}
			if err := updateAppleAccount(tx, &account, identity, ciphertext, version, s.now()); err != nil {
				return err
			}
			result = appleLoginUser{ID: account.UserID, Status: account.User.Status}
			return nil
		}
		if !errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return query.Error
		}
		now := s.now()
		user := newAppleUser(identity, now)
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		account = newAppleOAuthAccount(user.ID, identity, ciphertext, version, now)
		created := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "provider"}, {Name: "provider_subject"}}, DoNothing: true}).Create(&account)
		if created.Error != nil {
			return created.Error
		}
		if created.RowsAffected == 1 {
			result = appleLoginUser{ID: user.ID, Status: user.Status}
			return nil
		}
		if err := tx.Delete(&user).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Preload("User").Where("provider = ? AND provider_subject = ?", OAuthProviderApple, identity.Subject).Take(&account).Error; err != nil {
			return err
		}
		if account.User == nil {
			return errors.New("Apple OAuth account has no user")
		}
		if err := updateAppleAccount(tx, &account, identity, ciphertext, version, now); err != nil {
			return err
		}
		result = appleLoginUser{ID: account.UserID, Status: account.User.Status}
		return nil
	})
	if err != nil {
		return appleLoginUser{}, err
	}
	return result, nil
}

func newAppleUser(identity AppleIdentity, now time.Time) User {
	return User{ID: uuid.New(), Email: appleVerifiedEmail(identity), DisplayName: googleOptionalString(identity.DisplayName, 100), Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
}
func newAppleOAuthAccount(userID uuid.UUID, identity AppleIdentity, ciphertext []byte, version *int16, now time.Time) OAuthAccount {
	return OAuthAccount{ID: uuid.New(), UserID: userID, Provider: OAuthProviderApple, ProviderSubject: identity.Subject, ProviderEmail: googleOptionalString(identity.Email, 320), EmailVerified: identity.EmailVerified, IsPrivateEmail: identity.IsPrivateEmail, ProviderRefreshTokenCiphertext: ciphertext, ProviderTokenKeyVersion: version, LastLoginAt: now, CreatedAt: now, UpdatedAt: now}
}
func appleVerifiedEmail(identity AppleIdentity) *string {
	if !identity.EmailVerified {
		return nil
	}
	return googleOptionalString(identity.Email, 320)
}

func updateAppleAccount(tx *gorm.DB, account *OAuthAccount, identity AppleIdentity, ciphertext []byte, version *int16, now time.Time) error {
	updates := map[string]any{"provider_email": googleOptionalString(identity.Email, 320), "email_verified": identity.EmailVerified, "is_private_email": identity.IsPrivateEmail, "last_login_at": now, "updated_at": now}
	if len(ciphertext) > 0 {
		updates["provider_refresh_token_ciphertext"] = ciphertext
		updates["provider_token_key_version"] = version
	}
	if err := tx.Model(&OAuthAccount{}).Where("id = ?", account.ID).Updates(updates).Error; err != nil {
		return err
	}
	// Apple only sends a user's name during initial authorization, so never
	// overwrite the already persisted display name on later sign-ins.
	if identity.EmailVerified {
		return tx.Model(&User{}).Where("id = ?", account.UserID).Updates(map[string]any{"email": appleVerifiedEmail(identity), "updated_at": now}).Error
	}
	return nil
}

type AppleLoginService struct {
	exchanger AppleAuthorizationExchanger
	verifier  AppleIdentityVerifier
	accounts  AppleAccountResolver
	tokens    *TokenService
	cipher    *ProviderTokenCipher
}

func NewAppleLoginService(exchanger AppleAuthorizationExchanger, verifier AppleIdentityVerifier, accounts AppleAccountResolver, tokens *TokenService, cipher *ProviderTokenCipher) (*AppleLoginService, error) {
	if exchanger == nil || verifier == nil || accounts == nil || tokens == nil || cipher == nil {
		return nil, errors.New("Apple login dependencies must not be nil")
	}
	return &AppleLoginService{exchanger: exchanger, verifier: verifier, accounts: accounts, tokens: tokens, cipher: cipher}, nil
}
func (s *AppleLoginService) Login(ctx context.Context, authorizationCode, nonce, displayName, clientID string, client ClientInfo) (TokenPair, error) {
	response, err := s.exchanger.Exchange(ctx, authorizationCode, clientID)
	if err != nil {
		return TokenPair{}, fmt.Errorf("exchange Apple authorization code: %w", err)
	}
	identity, err := s.verifier.Verify(ctx, response.IDToken, nonce, clientID)
	if err != nil {
		if errors.Is(err, ErrInvalidAppleIDToken) {
			return TokenPair{}, err
		}
		return TokenPair{}, fmt.Errorf("verify Apple ID token: %w", err)
	}
	identity.DisplayName = appleDisplayName(displayName)
	var ciphertext []byte
	var version *int16
	if strings.TrimSpace(response.RefreshToken) != "" {
		ciphertext, version, err = s.cipher.Encrypt(response.RefreshToken)
		if err != nil {
			return TokenPair{}, err
		}
	}
	user, err := s.accounts.ResolveAppleIdentity(ctx, identity, ciphertext, version)
	if err != nil {
		return TokenPair{}, fmt.Errorf("resolve Apple account: %w", err)
	}
	if user.Status != UserStatusActive {
		return TokenPair{}, ErrUserInactive
	}
	return s.tokens.IssuePair(ctx, user.ID, client)
}

func appleDisplayName(value string) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	if utf8.RuneCountInString(value) <= 100 {
		return value
	}
	return string([]rune(value)[:100])
}
