package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestGoogleLoginRejectsUnlinkedIdentityWithoutCreatingUser(t *testing.T) {
	verifier := &stubGoogleVerifier{identity: GoogleIdentity{Subject: "new-subject", Email: "new@example.com", EmailVerified: true}}
	accounts := &stubGoogleAccounts{err: ErrAccountNotLinked}
	service, err := NewGoogleLoginService(verifier, accounts, testTokenService(newMemoryRefreshStore()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.LoginV2(context.Background(), "id-token", ClientInfo{}); !errors.Is(err, ErrAccountNotLinked) {
		t.Fatalf("unlinked login error = %v, want ErrAccountNotLinked", err)
	}
	if accounts.created {
		t.Fatal("v2 login asked the resolver to create a user")
	}

	handler := testGoogleLoginHTTPHandlerWithAccounts(t, verifier, accounts)
	response := serveEmailJSON(t, handler, http.MethodPost, "/auth/v2/google", map[string]string{"id_token": "id-token"}, nil)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"account_not_linked"`) {
		t.Fatalf("unlinked login HTTP = %d %s", response.Code, response.Body.String())
	}
}

func TestOAuthOnlyUserGetsSetupTokenInsteadOfLogin(t *testing.T) {
	userID := uuid.New()
	tokens := testTokenService(newMemoryRefreshStore())
	verifier := &stubGoogleVerifier{identity: GoogleIdentity{Subject: "legacy-subject"}}
	accounts := &stubGoogleAccounts{user: googleLoginUser{ID: userID, Status: UserStatusActive}}
	service, err := NewGoogleLoginService(verifier, accounts, tokens)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.LoginV2(context.Background(), "id-token", ClientInfo{})
	var setup *PasswordSetupRequiredError
	if !errors.As(err, &setup) || setup.SetupToken == "" {
		t.Fatalf("legacy login error = %v, want password setup", err)
	}
	if got, err := tokens.ValidateAccountSetupToken(setup.SetupToken); err != nil || got != userID {
		t.Fatalf("setup token user = %v, %v", got, err)
	}
	// setup 토큰은 API 접근에 쓸 수 없다.
	if _, err := tokens.ValidateAccessToken(setup.SetupToken); !errors.Is(err, ErrInvalidAccessToken) {
		t.Fatalf("setup token accepted as access token: %v", err)
	}

	handler := testGoogleLoginHTTPHandlerWithAccounts(t, verifier, accounts)
	response := serveEmailJSON(t, handler, http.MethodPost, "/auth/v2/google", map[string]string{"id_token": "id-token"}, nil)
	var body struct {
		Error       struct{ Code string } `json:"error"`
		SetupToken  string                `json:"setup_token"`
		AccessToken string                `json:"access_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusForbidden || body.Error.Code != "password_setup_required" || body.SetupToken == "" || body.AccessToken != "" {
		t.Fatalf("legacy login HTTP = %d %s", response.Code, response.Body.String())
	}
}

// v1(/auth/google)은 v2로 옮기지 않은 앱을 위해 기존 동작을 유지한다 — 처음 보는
// 신원이면 계정을 만들고, 이메일 계정 없는 기존 가입자도 그대로 로그인시킨다.
func TestV1GoogleLoginKeepsCreatingAndLoggingInLegacyUsers(t *testing.T) {
	verifier := &stubGoogleVerifier{identity: GoogleIdentity{Subject: "legacy-subject"}}
	accounts := &stubGoogleAccounts{user: googleLoginUser{ID: uuid.New(), Status: UserStatusActive}}
	handler := testGoogleLoginHTTPHandlerWithAccounts(t, verifier, accounts)

	response := serveEmailJSON(t, handler, http.MethodPost, "/auth/google", map[string]string{"id_token": "id-token"}, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "access_token") {
		t.Fatalf("v1 legacy login = %d %s", response.Code, response.Body.String())
	}
	if !accounts.created {
		t.Fatal("v1 login must let the resolver create a missing user")
	}
}

func TestAccountSetupTokenRejectsAccessTokenAndExpiry(t *testing.T) {
	tokens := testTokenService(newMemoryRefreshStore())
	userID := uuid.New()
	pair, err := tokens.IssuePair(context.Background(), userID, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.ValidateAccountSetupToken(pair.AccessToken); !errors.Is(err, ErrInvalidSetupToken) {
		t.Fatalf("access token accepted as setup token: %v", err)
	}
	setup, err := tokens.IssueAccountSetupToken(userID)
	if err != nil {
		t.Fatal(err)
	}
	tokens.now = func() time.Time { return time.Now().UTC().Add(accountSetupTokenTTL + time.Minute) }
	if _, err := tokens.ValidateAccountSetupToken(setup); !errors.Is(err, ErrInvalidSetupToken) {
		t.Fatalf("expired setup token error = %v", err)
	}
}

func TestAccountSetupAttachesEmailToExistingUser(t *testing.T) {
	pending := newMemoryPendingEmailSignupStore()
	accounts := &memoryEmailAccountStore{}
	sender := &recordingVerificationEmailSender{}
	tokens := testTokenService(newMemoryRefreshStore())
	service := newTestEmailAuthService(t, pending, accounts, sender, tokens)
	config, _ := NewTokenHTTPConfig(false, nil)
	handler := MountAuthHTTPWithServices(http.NotFoundHandler(), tokens, nil, nil, service, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), config)
	legacyUser := uuid.New()
	setupToken, err := tokens.IssueAccountSetupToken(legacyUser)
	if err != nil {
		t.Fatal(err)
	}

	start := serveEmailJSON(t, handler, http.MethodPost, "/auth/native/account-setup", map[string]string{"setup_token": setupToken, "email": "legacy@example.com", "password": "correct horse battery staple"}, nil)
	var started struct {
		SignupToken string `json:"signup_token"`
	}
	_ = json.Unmarshal(start.Body.Bytes(), &started)
	if start.Code != http.StatusOK || started.SignupToken == "" || sender.code == "" {
		t.Fatalf("account setup = %d %s", start.Code, start.Body.String())
	}
	verify := serveEmailJSON(t, handler, http.MethodPost, "/auth/native/verify-email", map[string]string{"signup_token": started.SignupToken, "verification_code": sender.code}, nil)
	if verify.Code != http.StatusOK {
		t.Fatalf("verify = %d %s", verify.Code, verify.Body.String())
	}
	if accounts.attachedTo != legacyUser || accounts.account == nil || accounts.account.UserID != legacyUser {
		t.Fatalf("email account attached to %v, want existing user %v", accounts.attachedTo, legacyUser)
	}
	login := serveEmailJSON(t, handler, http.MethodPost, "/auth/sign-in", map[string]string{"email": "legacy@example.com", "password": "correct horse battery staple"}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("sign-in after setup = %d %s", login.Code, login.Body.String())
	}
}

func TestAccountSetupRejectsBadTokenAndTakenEmail(t *testing.T) {
	pending := newMemoryPendingEmailSignupStore()
	taken := "owner@example.com"
	accounts := &memoryEmailAccountStore{user: &User{ID: uuid.New(), Email: &taken, Status: UserStatusActive}}
	sender := &recordingVerificationEmailSender{}
	tokens := testTokenService(newMemoryRefreshStore())
	service := newTestEmailAuthService(t, pending, accounts, sender, tokens)
	config, _ := NewTokenHTTPConfig(false, nil)
	handler := MountAuthHTTPWithServices(http.NotFoundHandler(), tokens, nil, nil, service, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), config)

	bad := serveEmailJSON(t, handler, http.MethodPost, "/auth/native/account-setup", map[string]string{"setup_token": "forged", "email": "x@example.com", "password": "correct horse battery staple"}, nil)
	if bad.Code != http.StatusUnauthorized || !strings.Contains(bad.Body.String(), "invalid_setup_token") {
		t.Fatalf("forged setup token = %d %s", bad.Code, bad.Body.String())
	}
	setupToken, _ := tokens.IssueAccountSetupToken(uuid.New())
	conflict := serveEmailJSON(t, handler, http.MethodPost, "/auth/native/account-setup", map[string]string{"setup_token": setupToken, "email": taken, "password": "correct horse battery staple"}, nil)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "email_already_registered") {
		t.Fatalf("taken email = %d %s", conflict.Code, conflict.Body.String())
	}
	if sender.code != "" {
		t.Fatal("rejected setup sent a verification email")
	}
}

func testGoogleLoginHTTPHandlerWithAccounts(t *testing.T, verifier GoogleIdentityVerifier, accounts GoogleAccountResolver) http.Handler {
	t.Helper()
	config, err := NewTokenHTTPConfig(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens := testTokenService(newMemoryRefreshStore())
	google, err := NewGoogleLoginService(verifier, accounts, tokens)
	if err != nil {
		t.Fatal(err)
	}
	return MountAuthHTTP(http.NotFoundHandler(), tokens, google, slog.New(slog.NewTextHandler(io.Discard, nil)), config)
}
