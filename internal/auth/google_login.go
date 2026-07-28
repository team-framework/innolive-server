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

// GoogleOAuthConfig contains the backend audience shared by web and Android.
// Android's own client ID identifies the signed application to Google, while
// ID tokens sent to this server must have the web client ID as their audience.
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

// GoogleIdentity is the verified identity information that can be persisted.
// The provider subject, rather than email, is the stable account identifier.
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

// ResolveGoogleIdentity finds the account by provider subject, never by email.
// For a new subject, the unique provider/subject index makes concurrent first
// logins converge on one OAuth account and one internal user.
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
		if query.Error == nil {
			if account.User == nil {
				return errors.New("Google OAuth account has no user")
			}
			if err := updateGoogleAccount(tx, &account, identity, s.now()); err != nil {
				return err
			}
			result = googleLoginUser{ID: account.UserID, Status: account.User.Status}
			return nil
		}
		if !errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return query.Error
		}

		now := s.now()
		user := newGoogleUser(identity, now)
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		account = newGoogleOAuthAccount(user.ID, identity, now)
		created := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "provider"}, {Name: "provider_subject"}},
			DoNothing: true,
		}).Create(&account)
		if created.Error != nil {
			return created.Error
		}
		if created.RowsAffected == 1 {
			result = googleLoginUser{ID: user.ID, Status: user.Status}
			return nil
		}

		// A concurrent request created the account. Remove the unreferenced user
		// inside this transaction, then use the account it won.
		if err := tx.Delete(&user).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Preload("User").
			Where("provider = ? AND provider_subject = ?", OAuthProviderGoogle, identity.Subject).
			Take(&account).Error; err != nil {
			return err
		}
		if account.User == nil {
			return errors.New("Google OAuth account has no user")
		}
		if err := updateGoogleAccount(tx, &account, identity, now); err != nil {
			return err
		}
		result = googleLoginUser{ID: account.UserID, Status: account.User.Status}
		return nil
	})
	if err != nil {
		return googleLoginUser{}, err
	}
	return result, nil
}

func newGoogleUser(identity GoogleIdentity, now time.Time) User {
	return User{
		ID:              uuid.New(),
		Email:           googleVerifiedEmail(identity),
		DisplayName:     googleOptionalString(identity.DisplayName, 100),
		ProfileImageURL: googleOptionalString(identity.ProfileURL, 0),
		Status:          UserStatusActive,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

func newGoogleOAuthAccount(userID uuid.UUID, identity GoogleIdentity, now time.Time) OAuthAccount {
	return OAuthAccount{
		ID:              uuid.New(),
		UserID:          userID,
		Provider:        OAuthProviderGoogle,
		ProviderSubject: identity.Subject,
		ProviderEmail:   googleOptionalString(identity.Email, 320),
		EmailVerified:   identity.EmailVerified,
		LastLoginAt:     now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
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
	userUpdates := map[string]any{
		"display_name":      googleOptionalString(identity.DisplayName, 100),
		"profile_image_url": googleOptionalString(identity.ProfileURL, 0),
		"updated_at":        now,
	}
	if identity.EmailVerified {
		userUpdates["email"] = googleVerifiedEmail(identity)
	}
	return tx.Model(&User{}).Where("id = ?", account.UserID).Updates(userUpdates).Error
}

func googleVerifiedEmail(identity GoogleIdentity) *string {
	if !identity.EmailVerified {
		return nil
	}
	return googleOptionalString(identity.Email, 320)
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
		return TokenPair{}, fmt.Errorf("resolve Google account: %w", err)
	}
	if user.Status != UserStatusActive {
		return TokenPair{}, ErrUserInactive
	}
	return s.tokens.IssuePair(ctx, user.ID, client)
}
