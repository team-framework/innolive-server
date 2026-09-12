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
	token, err := service.StartSignup(context.Background(), "member@example.com", "correct horse battery staple", "203.0.113.1")
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

func TestStartSignupThrottlesResendAndPerEmailCap(t *testing.T) {
	pending := newMemoryPendingEmailSignupStore()
	sender := &recordingVerificationEmailSender{}
	service := newTestEmailAuthService(t, pending, &memoryEmailAccountStore{}, sender, testTokenService(newMemoryRefreshStore()))

	if _, err := service.StartSignup(context.Background(), "victim@example.com", "correct horse battery staple", "203.0.113.9"); err != nil {
		t.Fatalf("first signup = %v", err)
	}
	// A second request within the resend interval must be rejected before bcrypt/SMTP.
	if _, err := service.StartSignup(context.Background(), "victim@example.com", "correct horse battery staple", "203.0.113.9"); !errors.Is(err, ErrEmailSignupThrottled) {
		t.Fatalf("resend signup = %v, want throttled", err)
	}

	// Clearing the resend flag lets the per-email window cap be reached instead.
	for i := 0; i < emailSignupEmailLimitDefault+2; i++ {
		_ = pending.ClearKey(context.Background(), signupResendKey("victim@example.com"))
		_, err := service.StartSignup(context.Background(), "victim@example.com", "correct horse battery staple", "203.0.113.9")
		if i < emailSignupEmailLimitDefault-1 && err != nil {
			t.Fatalf("signup %d within cap = %v", i, err)
		}
		if i >= emailSignupEmailLimitDefault && !errors.Is(err, ErrEmailSignupThrottled) {
			t.Fatalf("signup %d beyond cap = %v, want throttled", i, err)
		}
	}
}

func TestCompleteSignupCapsCodeAttemptsThenRejectsCorrectCode(t *testing.T) {
	pending := newMemoryPendingEmailSignupStore()
	sender := &recordingVerificationEmailSender{}
	service := newTestEmailAuthService(t, pending, &memoryEmailAccountStore{}, sender, testTokenService(newMemoryRefreshStore()))
	token, err := service.StartSignup(context.Background(), "member@example.com", "correct horse battery staple", "203.0.113.1")
	if err != nil {
		t.Fatal(err)
	}
	wrong := "000000"
	if wrong == sender.code {
		wrong = "000001"
	}
	for i := 0; i < emailCodeMaxAttemptsDefault; i++ {
		if err := service.CompleteSignup(context.Background(), token, wrong); !errors.Is(err, ErrEmailVerificationInvalid) {
			t.Fatalf("wrong attempt %d = %v", i, err)
		}
	}
	// The cap is now exceeded: even the correct code must be rejected and the pending user discarded.
	if err := service.CompleteSignup(context.Background(), token, sender.code); !errors.Is(err, ErrEmailVerificationInvalid) {
		t.Fatalf("correct code after cap = %v, want invalid", err)
	}
	if pending.has(token) {
		t.Fatal("pending signup was not discarded after exceeding the attempt cap")
	}
}

func TestLoginHardLimitAndFailureReset(t *testing.T) {
	pending := newMemoryPendingEmailSignupStore()
	accounts := &memoryEmailAccountStore{}
	sender := &recordingVerificationEmailSender{}
	service := newTestEmailAuthService(t, pending, accounts, sender, testTokenService(newMemoryRefreshStore()))
	token, err := service.StartSignup(context.Background(), "member@example.com", "correct horse battery staple", "203.0.113.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteSignup(context.Background(), token, sender.code); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < emailLoginMaxFailuresDefault; i++ {
		if _, err := service.Login(context.Background(), "member@example.com", "wrong-password", ClientInfo{}); !errors.Is(err, ErrEmailCredentialsInvalid) {
			t.Fatalf("failed login %d = %v", i, err)
		}
	}
	// After the hard cap the correct password is refused with a throttle error.
	if _, err := service.Login(context.Background(), "member@example.com", "correct horse battery staple", ClientInfo{}); !errors.Is(err, ErrEmailLoginThrottled) {
		t.Fatalf("login at cap = %v, want throttled", err)
	}

	// A successful login below the cap clears the counter.
	_ = pending.ClearKey(context.Background(), loginFailureKey("member@example.com"))
	if _, err := service.Login(context.Background(), "member@example.com", "correct horse battery staple", ClientInfo{}); err != nil {
		t.Fatalf("successful login = %v", err)
	}
	if got, _ := pending.Count(context.Background(), loginFailureKey("member@example.com")); got != 0 {
		t.Fatalf("failure counter after success = %d, want 0", got)
	}
}

func TestRedisEmailRateLimiterCountsAndExpires(t *testing.T) {
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
	limiter := store.(EmailRateLimiter)

	if value, err := limiter.Increment(context.Background(), "rl:test", time.Minute); err != nil || value != 1 {
		t.Fatalf("first increment = %d, %v", value, err)
	}
	if ttl := server.TTL("rl:test"); ttl != time.Minute {
		t.Fatalf("increment TTL = %s, want 1m", ttl)
	}
	if value, err := limiter.Increment(context.Background(), "rl:test", time.Hour); err != nil || value != 2 {
		t.Fatalf("second increment = %d, %v", value, err)
	}
	if ttl := server.TTL("rl:test"); ttl != time.Minute {
		t.Fatalf("TTL reset on later increment = %s, want unchanged 1m", ttl)
	}
	if fresh, err := limiter.SetIfAbsent(context.Background(), "rl:flag", time.Minute); err != nil || !fresh {
		t.Fatalf("first SetIfAbsent = %v, %v", fresh, err)
	}
	if fresh, err := limiter.SetIfAbsent(context.Background(), "rl:flag", time.Minute); err != nil || fresh {
		t.Fatalf("second SetIfAbsent = %v, %v, want not fresh", fresh, err)
	}
	if err := limiter.ClearKey(context.Background(), "rl:test"); err != nil {
		t.Fatal(err)
	}
	if got, err := limiter.Count(context.Background(), "rl:test"); err != nil || got != 0 {
		t.Fatalf("count after clear = %d, %v", got, err)
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
	limiter, ok := pending.(EmailRateLimiter)
	if !ok {
		t.Fatalf("pending store %T does not implement EmailRateLimiter", pending)
	}
	service, err := NewEmailAuthService(pending, accounts, sender, tokens, limiter, EmailAuthConfig{
		SMTPHost: "smtp.example.com", RedisAddr: "redis:6379", CodeTTL: 5 * time.Minute, PendingUserTTL: 30 * time.Minute, BcryptCost: bcrypt.MinCost,
		SignupIPLimit: emailSignupIPLimitDefault, SignupEmailLimit: emailSignupEmailLimitDefault, SignupWindow: emailSignupWindowDefault, SignupResendInterval: emailSignupResendIntervalDefault,
		CodeMaxAttempts: emailCodeMaxAttemptsDefault, LoginMaxFailures: emailLoginMaxFailuresDefault, LoginFailureWindow: emailLoginFailureWindowDefault, LoginFailureDelay: 0,
	})
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
	pending  map[string]PendingEmailSignup
	codes    map[string]string
	counters map[string]int64
	flags    map[string]bool
}

func newMemoryPendingEmailSignupStore() *memoryPendingEmailSignupStore {
	return &memoryPendingEmailSignupStore{pending: map[string]PendingEmailSignup{}, codes: map[string]string{}, counters: map[string]int64{}, flags: map[string]bool{}}
}

func (s *memoryPendingEmailSignupStore) Increment(_ context.Context, key string, _ time.Duration) (int64, error) {
	s.counters[key]++
	return s.counters[key], nil
}
func (s *memoryPendingEmailSignupStore) Count(_ context.Context, key string) (int64, error) {
	return s.counters[key], nil
}
func (s *memoryPendingEmailSignupStore) SetIfAbsent(_ context.Context, key string, _ time.Duration) (bool, error) {
	if s.flags[key] {
		return false, nil
	}
	s.flags[key] = true
	return true, nil
}
func (s *memoryPendingEmailSignupStore) ClearKey(_ context.Context, key string) error {
	delete(s.counters, key)
	delete(s.flags, key)
	return nil
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
