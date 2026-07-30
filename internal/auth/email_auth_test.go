package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

func TestEmailSignupClashContract(t *testing.T) {
	pending := newMemoryPendingEmailSignupStore()
	accounts := &memoryEmailAccountStore{}
	sender := &recordingVerificationEmailSender{}
	tokens := testTokenService(newMemoryRefreshStore())
	service := newTestEmailAuthService(t, pending, accounts, sender, tokens)
	config, _ := NewTokenHTTPConfig(false, nil)
	handler := MountAuthHTTPWithServices(http.NotFoundHandler(), tokens, nil, nil, service, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), config)

	response := serveEmailJSON(t, handler, http.MethodPost, "/auth/sign-up", map[string]string{"email": "member@example.com", "password": "correct horse battery staple"}, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "verification_email_sent") {
		t.Fatalf("signup = %d, %s", response.Code, response.Body.String())
	}
	cookie := response.Result().Cookies()
	if len(cookie) != 1 || cookie[0].Name != signupTokenCookieName || !cookie[0].HttpOnly || !cookie[0].Secure {
		t.Fatalf("signup cookie = %#v", cookie)
	}
	if accounts.user != nil || sender.code == "" || len(sender.code) != 6 {
		t.Fatal("signup created a user or did not create a six digit email code")
	}

	verify := serveEmailJSON(t, handler, http.MethodPost, "/auth/verify-email", map[string]string{"verification_code": sender.code}, cookie[0])
	if verify.Code != http.StatusOK || !strings.Contains(verify.Body.String(), "email_verified") {
		t.Fatalf("verify = %d, %s", verify.Code, verify.Body.String())
	}
	if accounts.user == nil || pending.has(cookie[0].Value) {
		t.Fatal("verification did not save the user then clean Redis state")
	}

	login := serveEmailJSON(t, handler, http.MethodPost, "/auth/sign-in", map[string]string{"email": "member@example.com", "password": "correct horse battery staple"}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("sign-in = %d, %s", login.Code, login.Body.String())
	}
}

func TestNativeEmailSignupUsesJSONSignupToken(t *testing.T) {
	pending := newMemoryPendingEmailSignupStore()
	accounts := &memoryEmailAccountStore{}
	sender := &recordingVerificationEmailSender{}
	tokens := testTokenService(newMemoryRefreshStore())
	service := newTestEmailAuthService(t, pending, accounts, sender, tokens)
	config, _ := NewTokenHTTPConfig(false, nil)
	handler := MountAuthHTTPWithServices(http.NotFoundHandler(), tokens, nil, nil, service, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), config)

	signup := serveEmailJSON(t, handler, http.MethodPost, "/auth/native/sign-up", map[string]string{"email": "native@example.com", "password": "correct horse battery staple"}, nil)
	if signup.Code != http.StatusOK {
		t.Fatalf("native signup = %d, %s", signup.Code, signup.Body.String())
	}
	if cookies := signup.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("native signup set cookies = %#v", cookies)
	}
	var signupResponse struct {
		Status      string `json:"status"`
		SignupToken string `json:"signup_token"`
	}
	if err := json.Unmarshal(signup.Body.Bytes(), &signupResponse); err != nil {
		t.Fatal(err)
	}
	if signupResponse.Status != "verification_email_sent" || signupResponse.SignupToken == "" {
		t.Fatalf("native signup response = %#v", signupResponse)
	}

	webVerify := serveEmailJSON(t, handler, http.MethodPost, "/auth/verify-email", map[string]string{"signup_token": signupResponse.SignupToken, "verification_code": sender.code}, nil)
	if webVerify.Code != http.StatusBadRequest || !strings.Contains(webVerify.Body.String(), "invalid_signup_token") {
		t.Fatalf("web verification accepted JSON token = %d, %s", webVerify.Code, webVerify.Body.String())
	}

	verify := serveEmailJSON(t, handler, http.MethodPost, "/auth/native/verify-email", map[string]string{"signup_token": signupResponse.SignupToken, "verification_code": sender.code}, nil)
	if verify.Code != http.StatusOK || !strings.Contains(verify.Body.String(), "email_verified") {
		t.Fatalf("native verify = %d, %s", verify.Code, verify.Body.String())
	}
	if accounts.user == nil || pending.has(signupResponse.SignupToken) {
		t.Fatal("native verification did not save the user then clean Redis state")
	}
}

func TestEmailSignupVerificationCodeIsConsumedOnce(t *testing.T) {
	pending := newMemoryPendingEmailSignupStore()
	accounts := &memoryEmailAccountStore{}
	sender := &recordingVerificationEmailSender{}
	service := newTestEmailAuthService(t, pending, accounts, sender, testTokenService(newMemoryRefreshStore()))
	token, err := service.StartSignup(context.Background(), "member@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteSignup(context.Background(), token, sender.code); err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteSignup(context.Background(), token, sender.code); !errors.Is(err, ErrEmailVerificationInvalid) {
		t.Fatalf("second verification = %v", err)
	}
}

func TestRedisPendingEmailSignupStoreUsesClashKeysAndTTL(t *testing.T) {
	server, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	store, err := NewRedisPendingEmailSignupStore(context.Background(), EmailAuthConfig{SMTPHost: "smtp.example.com", RedisAddr: server.Addr()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pending := PendingEmailSignup{Email: "member@example.com", PasswordHash: "password-hash"}
	if err := store.Save(context.Background(), "signup-token", pending, "code-hash", 30*time.Minute, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if !server.Exists("pending_user:signup-token") || !server.Exists("token:signup-token") {
		t.Fatal("Clash-compatible Redis keys were not written")
	}
	if ttl := server.TTL("token:signup-token"); ttl != 5*time.Minute {
		t.Fatalf("code TTL = %s, want 5m", ttl)
	}
	if got, err := store.ConsumeVerificationCode(context.Background(), "signup-token"); err != nil || got != "code-hash" {
		t.Fatalf("consume code = %q, %v", got, err)
	}
	if server.Exists("token:signup-token") {
		t.Fatal("consumed verification code remains in Redis")
	}
}

func TestLoadEmailAuthConfigFromEnv(t *testing.T) {
	for _, key := range []string{"AUTH_EMAIL_SMTP_HOST", "AUTH_EMAIL_SMTP_PORT", "AUTH_EMAIL_SMTP_USERNAME", "AUTH_EMAIL_SMTP_PASSWORD", "AUTH_EMAIL_SMTP_STARTTLS", "AUTH_EMAIL_SMTP_IMPLICIT_TLS", "AUTH_EMAIL_SENDER_ADDRESS", "AUTH_EMAIL_REDIS_ADDR", "AUTH_EMAIL_REDIS_USERNAME", "AUTH_EMAIL_REDIS_PASSWORD", "AUTH_EMAIL_REDIS_DB", "AUTH_EMAIL_VERIFICATION_CODE_TTL", "AUTH_EMAIL_PENDING_USER_TTL"} {
		t.Setenv(key, "")
	}
	config, err := LoadEmailAuthConfigFromEnv()
	if err != nil || config.Enabled() {
		t.Fatalf("empty config = %#v, %v", config, err)
	}
	t.Setenv("AUTH_EMAIL_SMTP_HOST", "smtp.example.com")
	t.Setenv("AUTH_EMAIL_SMTP_USERNAME", "user")
	t.Setenv("AUTH_EMAIL_SMTP_PASSWORD", "password")
	t.Setenv("AUTH_EMAIL_SENDER_ADDRESS", "no-reply@example.com")
	t.Setenv("AUTH_EMAIL_REDIS_ADDR", "redis:6379")
	config, err = LoadEmailAuthConfigFromEnv()
	if err != nil || !config.Enabled() || config.CodeTTL != 5*time.Minute || config.PendingUserTTL != 30*time.Minute {
		t.Fatalf("config = %#v, %v", config, err)
	}
}

func newTestEmailAuthService(t *testing.T, pending PendingEmailSignupStore, accounts EmailAccountStore, sender VerificationEmailSender, tokens *TokenService) *EmailAuthService {
	t.Helper()
	service, err := NewEmailAuthService(pending, accounts, sender, tokens, EmailAuthConfig{SMTPHost: "smtp.example.com", RedisAddr: "redis:6379", CodeTTL: 5 * time.Minute, PendingUserTTL: 30 * time.Minute, BcryptCost: bcrypt.MinCost})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func serveEmailJSON(t *testing.T, handler http.Handler, method, path string, value any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type recordingVerificationEmailSender struct{ code string }

func (s *recordingVerificationEmailSender) SendVerificationCode(_ context.Context, _ string, code string) error {
	s.code = code
	return nil
}

type memoryPendingEmailSignupStore struct {
	pending map[string]PendingEmailSignup
	codes   map[string]string
}

func newMemoryPendingEmailSignupStore() *memoryPendingEmailSignupStore {
	return &memoryPendingEmailSignupStore{pending: map[string]PendingEmailSignup{}, codes: map[string]string{}}
}
func (s *memoryPendingEmailSignupStore) Save(_ context.Context, token string, pending PendingEmailSignup, code string, _ time.Duration, _ time.Duration) error {
	s.pending[token] = pending
	s.codes[token] = code
	return nil
}
func (s *memoryPendingEmailSignupStore) PendingUser(_ context.Context, token string) (PendingEmailSignup, error) {
	value, ok := s.pending[token]
	if !ok {
		return PendingEmailSignup{}, ErrEmailVerificationInvalid
	}
	return value, nil
}
func (s *memoryPendingEmailSignupStore) VerificationCodeHash(_ context.Context, token string) (string, error) {
	value, ok := s.codes[token]
	if !ok {
		return "", ErrEmailVerificationInvalid
	}
	return value, nil
}
func (s *memoryPendingEmailSignupStore) ConsumeVerificationCode(_ context.Context, token string) (string, error) {
	value, ok := s.codes[token]
	if !ok {
		return "", ErrEmailVerificationInvalid
	}
	delete(s.codes, token)
	return value, nil
}
func (s *memoryPendingEmailSignupStore) Delete(_ context.Context, token string) error {
	delete(s.pending, token)
	delete(s.codes, token)
	return nil
}
func (s *memoryPendingEmailSignupStore) Close() error          { return nil }
func (s *memoryPendingEmailSignupStore) has(token string) bool { _, ok := s.pending[token]; return ok }

type memoryEmailAccountStore struct {
	user    *User
	account *EmailAccount
}

func (s *memoryEmailAccountStore) EmailAlreadyRegistered(_ context.Context, email string) (bool, error) {
	return s.user != nil && s.user.Email != nil && *s.user.Email == email, nil
}
func (s *memoryEmailAccountStore) CreateEmailUser(_ context.Context, pending PendingEmailSignup, now time.Time) (uuid.UUID, error) {
	exists, _ := s.EmailAlreadyRegistered(context.Background(), pending.Email)
	if exists {
		return uuid.Nil, ErrEmailAlreadyRegistered
	}
	user := User{ID: uuid.New(), Email: &pending.Email, Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
	account := EmailAccount{UserID: user.ID, Email: pending.Email, PasswordHash: pending.PasswordHash, CreatedAt: now, UpdatedAt: now}
	s.user, s.account = &user, &account
	return user.ID, nil
}
func (s *memoryEmailAccountStore) FindEmailAccount(_ context.Context, email string) (EmailAccount, User, error) {
	if s.account == nil || s.account.Email != email {
		return EmailAccount{}, User{}, ErrEmailCredentialsInvalid
	}
	return *s.account, *s.user, nil
}
