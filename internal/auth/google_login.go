package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/api/idtoken"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrInvalidGoogleIDToken = errors.New("invalid Google ID token")

// GoogleOAuthConfig는 웹과 Android가 함께 쓰는 백엔드 audience를 담는다. Android의
// 자체 client ID는 서명된 앱을 Google에 식별하고, 이 서버로 오는 ID token은 웹
// client ID를 audience로 가져야 한다.
type GoogleOAuthConfig struct {
	WebClientID string
}

func LoadGoogleOAuthConfigFromEnv() (GoogleOAuthConfig, error) {
	clientID := strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_WEB_CLIENT_ID"))
	if clientID == "" {
		return GoogleOAuthConfig{}, nil
	}
	if len(clientID) > 512 {
		return GoogleOAuthConfig{}, errors.New("GOOGLE_OAUTH_WEB_CLIENT_ID is too long")
	}
	return GoogleOAuthConfig{WebClientID: clientID}, nil
}

func (c GoogleOAuthConfig) Enabled() bool {
	return c.WebClientID != ""
}

// GoogleIdentity는 저장할 수 있는 검증된 신원 정보다. 이메일이 아니라 플랫폼
// subject가 안정적인 계정 식별자다.
type GoogleIdentity struct {
	Subject       string
	Email         string
	EmailVerified bool
	DisplayName   string
	ProfileURL    string
}

type GoogleIdentityVerifier interface {
	Verify(context.Context, string) (GoogleIdentity, error)
}

type googleIDTokenVerifier struct {
	validator *idtoken.Validator
	audience  string
}

func NewGoogleIDTokenVerifier(ctx context.Context, config GoogleOAuthConfig) (GoogleIdentityVerifier, error) {
	if !config.Enabled() {
		return nil, errors.New("Google OAuth is not configured")
	}
	validator, err := idtoken.NewValidator(ctx)
	if err != nil {
		return nil, fmt.Errorf("create Google ID token validator: %w", err)
	}
	return &googleIDTokenVerifier{validator: validator, audience: config.WebClientID}, nil
}

func (v *googleIDTokenVerifier) Verify(ctx context.Context, rawToken string) (GoogleIdentity, error) {
	payload, err := v.validator.Validate(ctx, strings.TrimSpace(rawToken), v.audience)
	if err != nil {
		return GoogleIdentity{}, fmt.Errorf("%w: %v", ErrInvalidGoogleIDToken, err)
	}
	if payload.Issuer != "https://accounts.google.com" && payload.Issuer != "accounts.google.com" {
		return GoogleIdentity{}, ErrInvalidGoogleIDToken
	}

	identity := GoogleIdentity{
		Subject:       googleClaimString(payload.Subject),
		Email:         googleClaimString(payload.Claims["email"]),
		EmailVerified: googleClaimBool(payload.Claims["email_verified"]),
		DisplayName:   googleClaimString(payload.Claims["name"]),
		ProfileURL:    googleClaimString(payload.Claims["picture"]),
	}
	if err := identity.Validate(); err != nil {
		return GoogleIdentity{}, err
	}
	return identity, nil
}

func (i GoogleIdentity) Validate() error {
	if i.Subject == "" || utf8.RuneCountInString(i.Subject) > 255 {
		return ErrInvalidGoogleIDToken
	}
	if utf8.RuneCountInString(i.Email) > 320 {
		return ErrInvalidGoogleIDToken
	}
	return nil
}

func googleClaimString(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(strings.ToValidUTF8(text, ""))
}

func googleClaimBool(value any) bool {
	verified, ok := value.(bool)
	return ok && verified
}

type googleLoginUser struct {
	ID     uuid.UUID
	Status UserStatus
	// HasEmailAccount가 false면 #380 이전의 구글 전용 가입자다. 로그인 대신 이메일
	// 계정 설정을 요구한다.
	HasEmailAccount bool
}

type GoogleAccountResolver interface {
	ResolveGoogleIdentity(context.Context, GoogleIdentity) (googleLoginUser, error)
}

type gormGoogleAccountResolver struct {
	db  *gorm.DB
	now func() time.Time
}

func NewGormGoogleAccountResolver(db *gorm.DB) GoogleAccountResolver {
	return &gormGoogleAccountResolver{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// ResolveGoogleIdentity는 이메일이 아니라 플랫폼 subject로 연결된 계정을 찾는다.
// 연결되지 않은 subject면 사용자를 만들지 않고 ErrAccountNotLinked다(#380) — 구글은
// 로그인한 InnoLive 계정에 연결한 뒤에만 로그인 수단이 된다.
func (s *gormGoogleAccountResolver) ResolveGoogleIdentity(ctx context.Context, identity GoogleIdentity) (googleLoginUser, error) {
	if s == nil || s.db == nil {
		return googleLoginUser{}, errors.New("Google account database is nil")
	}
	if err := identity.Validate(); err != nil {
		return googleLoginUser{}, err
	}

	var result googleLoginUser
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var account OAuthAccount
		query := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Preload("User").
			Where("provider = ? AND provider_subject = ?", OAuthProviderGoogle, identity.Subject).
			Take(&account)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return ErrAccountNotLinked
		}
		if query.Error != nil {
			return query.Error
		}
		if account.User == nil {
			return errors.New("Google OAuth account has no user")
		}
		if err := updateGoogleAccount(tx, &account, identity, s.now()); err != nil {
			return err
		}
		hasEmail, err := userHasEmailAccount(tx, account.UserID)
		if err != nil {
			return err
		}
		result = googleLoginUser{ID: account.UserID, Status: account.User.Status, HasEmailAccount: hasEmail}
		return nil
	})
	if err != nil {
		return googleLoginUser{}, err
	}
	return result, nil
}

func updateGoogleAccount(tx *gorm.DB, account *OAuthAccount, identity GoogleIdentity, now time.Time) error {
	accountUpdates := map[string]any{
		"provider_email": googleOptionalString(identity.Email, 320),
		"email_verified": identity.EmailVerified,
		"last_login_at":  now,
		"updated_at":     now,
	}
	if err := tx.Model(&OAuthAccount{}).Where("id = ?", account.ID).Updates(accountUpdates).Error; err != nil {
		return err
	}
	// 이메일 계정이 있으면 users.email은 그 계정의 이메일이 정본이다. 구글 이메일로
	// 덮지 않고 표시 이름·사진만 갱신한다.
	userUpdates := map[string]any{
		"display_name":      googleOptionalString(identity.DisplayName, 100),
		"profile_image_url": googleOptionalString(identity.ProfileURL, 0),
		"updated_at":        now,
	}
	return tx.Model(&User{}).Where("id = ?", account.UserID).Updates(userUpdates).Error
}

func googleOptionalString(value string, limit int) *string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	if limit > 0 && utf8.RuneCountInString(value) > limit {
		runes := []rune(value)
		value = string(runes[:limit])
	}
	if value == "" {
		return nil
	}
	return &value
}

type GoogleLoginService struct {
	verifier GoogleIdentityVerifier
	accounts GoogleAccountResolver
	tokens   *TokenService
	links    *AccountLinkStore
}

func NewGoogleLoginService(verifier GoogleIdentityVerifier, accounts GoogleAccountResolver, tokens *TokenService) (*GoogleLoginService, error) {
	if verifier == nil || accounts == nil || tokens == nil {
		return nil, errors.New("Google login dependencies must not be nil")
	}
	return &GoogleLoginService{verifier: verifier, accounts: accounts, tokens: tokens}, nil
}

func (s *GoogleLoginService) Login(ctx context.Context, rawIDToken string, client ClientInfo) (TokenPair, error) {
	identity, err := s.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		if errors.Is(err, ErrInvalidGoogleIDToken) {
			return TokenPair{}, ErrInvalidGoogleIDToken
		}
		return TokenPair{}, fmt.Errorf("verify Google ID token: %w", err)
	}
	user, err := s.accounts.ResolveGoogleIdentity(ctx, identity)
	if err != nil {
		if errors.Is(err, ErrAccountNotLinked) {
			return TokenPair{}, err
		}
		return TokenPair{}, fmt.Errorf("resolve Google account: %w", err)
	}
	if user.Status != UserStatusActive {
		return TokenPair{}, ErrUserInactive
	}
	if !user.HasEmailAccount {
		return TokenPair{}, s.tokens.passwordSetupRequired(user.ID)
	}
	return s.tokens.IssuePair(ctx, user.ID, client)
}

// SetAccountLinks는 로그인한 사용자에 구글을 연결하는 저장소를 붙인다(#380).
func (s *GoogleLoginService) SetAccountLinks(links *AccountLinkStore) { s.links = links }

// Link는 구글 ID 토큰을 검증해 로그인한 사용자에 연결한다.
func (s *GoogleLoginService) Link(ctx context.Context, userID uuid.UUID, rawIDToken string) (LinkResult, error) {
	if s.links == nil {
		return LinkResult{}, errors.New("Google account linking is not configured")
	}
	identity, err := s.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		if errors.Is(err, ErrInvalidGoogleIDToken) {
			return LinkResult{}, ErrInvalidGoogleIDToken
		}
		return LinkResult{}, fmt.Errorf("verify Google ID token: %w", err)
	}
	return s.links.Link(ctx, userID, LinkIdentity{
		Provider:      OAuthProviderGoogle,
		Subject:       identity.Subject,
		Email:         identity.Email,
		EmailVerified: identity.EmailVerified,
	})
}
