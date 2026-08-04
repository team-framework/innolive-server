package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type stubAppleExchanger struct {
	response AppleTokenResponse
	err      error
	seen     string
	clientID string
}

func (s *stubAppleExchanger) Exchange(_ context.Context, code, clientID string) (AppleTokenResponse, error) {
	s.seen, s.clientID = code, clientID
	return s.response, s.err
}

type stubAppleVerifier struct {
	identity  AppleIdentity
	err       error
	seenID    string
	seenNonce string
	clientID  string
}

func (s *stubAppleVerifier) Verify(_ context.Context, idToken, nonce, clientID string) (AppleIdentity, error) {
	s.seenID, s.seenNonce, s.clientID = idToken, nonce, clientID
	return s.identity, s.err
}

type stubAppleAccounts struct {
	user       appleLoginUser
	err        error
	identity   AppleIdentity
	ciphertext []byte
	version    *int16
}

func (s *stubAppleAccounts) ResolveAppleIdentity(_ context.Context, identity AppleIdentity, ciphertext []byte, version *int16) (appleLoginUser, error) {
	s.identity, s.ciphertext, s.version = identity, ciphertext, version
	return s.user, s.err
}

func TestAppleLoginIssuesTokensAndEncryptsProviderRefreshToken(t *testing.T) {
	cipher := &ProviderTokenCipher{key: []byte("0123456789abcdef0123456789abcdef")}
	exchanger := &stubAppleExchanger{response: AppleTokenResponse{IDToken: "apple-id-token", RefreshToken: "apple-refresh-token"}}
	verifier := &stubAppleVerifier{identity: AppleIdentity{Subject: "apple-stable-subject", Email: "person@privaterelay.appleid.com", EmailVerified: true, IsPrivateEmail: true}}
	accounts := &stubAppleAccounts{user: appleLoginUser{ID: uuid.New(), Status: UserStatusActive}}
	service, err := NewAppleLoginService(exchanger, verifier, accounts, testTokenService(newMemoryRefreshStore()), cipher)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := service.Login(context.Background(), "authorization-code", "nonce-value", "Ada Lovelace", "com.framework.innolive", ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatalf("incomplete token pair: %+v", pair)
	}
	if exchanger.seen != "authorization-code" || exchanger.clientID != "com.framework.innolive" || verifier.seenID != "apple-id-token" || verifier.seenNonce != "nonce-value" || verifier.clientID != "com.framework.innolive" {
		t.Fatalf("unexpected exchange/verifier inputs: code=%q exchange_client_id=%q id=%q nonce=%q verify_client_id=%q", exchanger.seen, exchanger.clientID, verifier.seenID, verifier.seenNonce, verifier.clientID)
	}
	if accounts.identity.DisplayName != "Ada Lovelace" || len(accounts.ciphertext) == 0 || accounts.version == nil {
		t.Fatalf("resolver data = %+v ciphertext=%d version=%v", accounts.identity, len(accounts.ciphertext), accounts.version)
	}
	plaintext, err := cipher.Decrypt(accounts.ciphertext, accounts.version)
	if err != nil || plaintext != "apple-refresh-token" {
		t.Fatalf("decrypt stored refresh token = %q, %v", plaintext, err)
	}
}

func TestAppleLoginRejectsInvalidIDTokenAndInactiveUser(t *testing.T) {
	cipher := &ProviderTokenCipher{key: []byte("0123456789abcdef0123456789abcdef")}
	invalid, err := NewAppleLoginService(
		&stubAppleExchanger{response: AppleTokenResponse{IDToken: "forged"}},
		&stubAppleVerifier{err: ErrInvalidAppleIDToken},
		&stubAppleAccounts{user: appleLoginUser{ID: uuid.New(), Status: UserStatusActive}},
		testTokenService(newMemoryRefreshStore()), cipher,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invalid.Login(context.Background(), "code", "", "", "com.framework.innolive", ClientInfo{}); !errors.Is(err, ErrInvalidAppleIDToken) {
		t.Fatalf("invalid ID token error = %v", err)
	}
	inactive, err := NewAppleLoginService(
		&stubAppleExchanger{response: AppleTokenResponse{IDToken: "valid"}},
		&stubAppleVerifier{identity: AppleIdentity{Subject: "apple-subject"}},
		&stubAppleAccounts{user: appleLoginUser{ID: uuid.New(), Status: UserStatusDisabled}},
		testTokenService(newMemoryRefreshStore()), cipher,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inactive.Login(context.Background(), "code", "", "", "com.framework.innolive", ClientInfo{}); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("inactive user error = %v", err)
	}
}

func TestAppleIDTokenVerifierChecksSignatureClaimsAndNonce(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	client := &appleOAuthClient{
		config:    AppleOAuthConfig{ClientID: "com.framework.innolive"},
		keys:      map[string]*rsa.PublicKey{"apple-kid": &privateKey.PublicKey},
		keysUntil: now.Add(time.Hour), now: func() time.Time { return now },
	}
	claims := appleIDTokenClaims{
		Email: "person@example.com", EmailVerified: "true", IsPrivateEmail: "false", Nonce: "expected-nonce",
		RegisteredClaims: jwt.RegisteredClaims{Issuer: appleIssuer, Subject: "apple-subject", Audience: jwt.ClaimStrings{"com.framework.innolive"}, ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "apple-kid"
	raw, err := token.SignedString(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := client.Verify(context.Background(), raw, "expected-nonce", "com.framework.innolive")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Subject != "apple-subject" || !identity.EmailVerified || identity.IsPrivateEmail {
		t.Fatalf("identity = %+v", identity)
	}
	if _, err := client.Verify(context.Background(), raw, "wrong-nonce", "com.framework.innolive"); !errors.Is(err, ErrInvalidAppleIDToken) {
		t.Fatalf("nonce mismatch error = %v", err)
	}
}

func TestAppleOAuthConfigOnlyAcceptsConfiguredClientID(t *testing.T) {
	config := AppleOAuthConfig{ClientID: "com.framework.innolive"}
	if !config.SupportsClientID("com.framework.innolive") || config.SupportsClientID("com.framework.innolive.macos") {
		t.Fatalf("configured client ID validation failed")
	}
}

func TestAppleJWKParsesRSAPublicKey(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key := (appleJWK{
		KTY: "RSA", KID: "apple-kid", ALG: "RS256",
		N: base64.RawURLEncoding.EncodeToString(privateKey.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
	}).publicKey()
	if key == nil || key.E != privateKey.E || key.N.Cmp(privateKey.N) != 0 {
		t.Fatalf("parsed RSA key = %+v", key)
	}
	if (&appleJWK{KTY: "EC", KID: "apple-kid", ALG: "ES256"}).publicKey() != nil {
		t.Fatal("EC JWK must not be accepted")
	}
}

func TestAppleClientSecretUsesES256AppleClaims(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	client := &appleOAuthClient{config: AppleOAuthConfig{TeamID: "TEAMID", ClientID: "com.framework.innolive", KeyID: "KEYID"}, privateKey: privateKey, now: func() time.Time { return now }}
	raw, err := client.clientSecret("com.framework.innolive")
	if err != nil {
		t.Fatal(err)
	}
	claims := &jwt.RegisteredClaims{}
	parsed, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodES256 || token.Header["kid"] != "KEYID" {
			return nil, errors.New("unexpected client secret header")
		}
		return &privateKey.PublicKey, nil
	})
	if err != nil || !parsed.Valid || claims.Issuer != "TEAMID" || claims.Subject != "com.framework.innolive" || len(claims.Audience) != 1 || claims.Audience[0] != appleIssuer {
		t.Fatalf("client secret claims=%+v valid=%v err=%v", claims, parsed != nil && parsed.Valid, err)
	}
}
