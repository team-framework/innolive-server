package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type stubGoogleVerifier struct {
	identity GoogleIdentity
	err      error
	seen     string
}

func (v *stubGoogleVerifier) Verify(_ context.Context, rawToken string) (GoogleIdentity, error) {
	v.seen = rawToken
	return v.identity, v.err
}

type stubGoogleAccounts struct {
	user googleLoginUser
	err  error
	seen GoogleIdentity
}

func (a *stubGoogleAccounts) ResolveGoogleIdentity(_ context.Context, identity GoogleIdentity) (googleLoginUser, error) {
	a.seen = identity
	return a.user, a.err
}

func TestGoogleLoginIssuesInnoLivePairForVerifiedSubject(t *testing.T) {
	userID := uuid.New()
	verifier := &stubGoogleVerifier{identity: GoogleIdentity{
		Subject:       "google-stable-subject",
		Email:         "person@example.com",
		EmailVerified: true,
		DisplayName:   "Inno User",
	}}
	accounts := &stubGoogleAccounts{user: googleLoginUser{ID: userID, Status: UserStatusActive}}
	service, err := NewGoogleLoginService(verifier, accounts, testTokenService(newMemoryRefreshStore()))
	if err != nil {
		t.Fatal(err)
	}

	pair, err := service.Login(context.Background(), "google-id-token", ClientInfo{UserAgent: "test-client"})
	if err != nil {
		t.Fatal(err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatalf("issued pair is incomplete: %+v", pair)
	}
	if verifier.seen != "google-id-token" {
		t.Fatalf("verifier saw %q", verifier.seen)
	}
	if accounts.seen.Subject != verifier.identity.Subject {
		t.Fatalf("stored subject = %q", accounts.seen.Subject)
	}
}

func TestGoogleLoginRejectsInvalidTokenAndInactiveUser(t *testing.T) {
	invalidVerifier := &stubGoogleVerifier{err: ErrInvalidGoogleIDToken}
	activeAccounts := &stubGoogleAccounts{user: googleLoginUser{ID: uuid.New(), Status: UserStatusActive}}
	invalidService, err := NewGoogleLoginService(invalidVerifier, activeAccounts, testTokenService(newMemoryRefreshStore()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invalidService.Login(context.Background(), "forged", ClientInfo{}); !errors.Is(err, ErrInvalidGoogleIDToken) {
		t.Fatalf("invalid token error = %v", err)
	}

	inactiveVerifier := &stubGoogleVerifier{identity: GoogleIdentity{Subject: "google-subject"}}
	inactiveAccounts := &stubGoogleAccounts{user: googleLoginUser{ID: uuid.New(), Status: UserStatusDisabled}}
	inactiveService, err := NewGoogleLoginService(inactiveVerifier, inactiveAccounts, testTokenService(newMemoryRefreshStore()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inactiveService.Login(context.Background(), "valid-but-disabled", ClientInfo{}); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("inactive user error = %v", err)
	}
}

func TestGoogleOAuthConfigUsesWebClientIDAsAudience(t *testing.T) {
	t.Setenv("GOOGLE_OAUTH_WEB_CLIENT_ID", "web-client.apps.googleusercontent.com")
	config, err := LoadGoogleOAuthConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled() || config.WebClientID != "web-client.apps.googleusercontent.com" {
		t.Fatalf("config = %+v", config)
	}
}
