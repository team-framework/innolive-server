package auth

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const (
	emailVerificationCodeDigits = 6
	pendingUserKeyPrefix        = "pending_user:"
	verificationCodeKeyPrefix   = "token:"

	signupIPRateKeyPrefix    = "rl:signup:ip:"
	signupEmailRateKeyPrefix = "rl:signup:email:"
	signupResendKeyPrefix    = "rl:signup:resend:"
	codeAttemptKeyPrefix     = "rl:code:attempt:"
	loginFailureKeyPrefix    = "rl:login:fail:"

	emailSignupIPLimitDefault        = 10
	emailSignupEmailLimitDefault     = 5
	emailSignupWindowDefault         = time.Hour
	emailSignupResendIntervalDefault = time.Minute
	emailCodeMaxAttemptsDefault      = 5
	emailLoginMaxFailuresDefault     = 15
	emailLoginFailureWindowDefault   = 15 * time.Minute
	emailLoginFailureDelayDefault    = 100 * time.Millisecond
	emailLoginMaxDelay               = 2 * time.Second
)

var (
	ErrEmailAlreadyRegistered   = errors.New("email already registered")
	ErrEmailSignupInvalid       = errors.New("email signup is invalid")
	ErrEmailSignupThrottled     = errors.New("email signup is throttled")
	ErrEmailVerificationInvalid = errors.New("email verification is invalid")
	ErrEmailCredentialsInvalid  = errors.New("email credentials are invalid")
	ErrEmailLoginThrottled      = errors.New("email login is throttled")
	ErrEmailDeliveryUnavailable = errors.New("email delivery is unavailable")
)

// EmailAuthConfig mirrors the short-lived signup contract used by Clash:
// a verification code lasts five minutes and its pending user lasts 30 minutes.
// Email authentication is disabled when AUTH_EMAIL_SMTP_HOST is empty.
type EmailAuthConfig struct {
	SMTPHost       string
	SMTPPort       int
	SMTPUsername   string
	SMTPPassword   string
	SenderAddress  string
	SenderName     string
	StartTLS       bool
	ImplicitTLS    bool
	RedisAddr      string
	RedisUsername  string
	RedisPassword  string
	RedisDB        int
	CodeTTL        time.Duration
	PendingUserTTL time.Duration
	BcryptCost     int

	SignupIPLimit        int
	SignupEmailLimit     int
	SignupWindow         time.Duration
	SignupResendInterval time.Duration
	CodeMaxAttempts      int
	LoginMaxFailures     int
	LoginFailureWindow   time.Duration
	LoginFailureDelay    time.Duration
}

func LoadEmailAuthConfigFromEnv() (EmailAuthConfig, error) {
	config := EmailAuthConfig{
		SMTPHost:       strings.TrimSpace(os.Getenv("AUTH_EMAIL_SMTP_HOST")),
		SMTPUsername:   strings.TrimSpace(os.Getenv("AUTH_EMAIL_SMTP_USERNAME")),
		SMTPPassword:   os.Getenv("AUTH_EMAIL_SMTP_PASSWORD"),
		SenderAddress:  strings.TrimSpace(os.Getenv("AUTH_EMAIL_SENDER_ADDRESS")),
		SenderName:     strings.TrimSpace(os.Getenv("AUTH_EMAIL_SENDER_NAME")),
		RedisAddr:      strings.TrimSpace(os.Getenv("AUTH_EMAIL_REDIS_ADDR")),
		RedisUsername:  strings.TrimSpace(os.Getenv("AUTH_EMAIL_REDIS_USERNAME")),
		RedisPassword:  os.Getenv("AUTH_EMAIL_REDIS_PASSWORD"),
		CodeTTL:        5 * time.Minute,
		PendingUserTTL: 30 * time.Minute,
		BcryptCost:     bcrypt.DefaultCost,

		SignupIPLimit:        emailSignupIPLimitDefault,
		SignupEmailLimit:     emailSignupEmailLimitDefault,
		SignupWindow:         emailSignupWindowDefault,
		SignupResendInterval: emailSignupResendIntervalDefault,
		CodeMaxAttempts:      emailCodeMaxAttemptsDefault,
		LoginMaxFailures:     emailLoginMaxFailuresDefault,
		LoginFailureWindow:   emailLoginFailureWindowDefault,
		LoginFailureDelay:    emailLoginFailureDelayDefault,
	}
	if config.SMTPHost == "" {
		return config, nil
	}

	var err error
	if config.StartTLS, err = emailBoolEnv("AUTH_EMAIL_SMTP_STARTTLS", true); err != nil {
		return EmailAuthConfig{}, err
	}
	if config.ImplicitTLS, err = emailBoolEnv("AUTH_EMAIL_SMTP_IMPLICIT_TLS", false); err != nil {
		return EmailAuthConfig{}, err
	}
	if config.SMTPPort, err = emailIntEnv("AUTH_EMAIL_SMTP_PORT", 587); err != nil || config.SMTPPort < 1 || config.SMTPPort > 65535 {
		return EmailAuthConfig{}, errors.New("AUTH_EMAIL_SMTP_PORT must be a valid TCP port")
	}
	if config.RedisDB, err = emailIntEnv("AUTH_EMAIL_REDIS_DB", 0); err != nil || config.RedisDB < 0 {
		return EmailAuthConfig{}, errors.New("AUTH_EMAIL_REDIS_DB must be a non-negative integer")
	}
	if config.CodeTTL, err = tokenEnvDuration("AUTH_EMAIL_VERIFICATION_CODE_TTL", config.CodeTTL); err != nil || config.CodeTTL <= 0 {
		return EmailAuthConfig{}, errors.New("AUTH_EMAIL_VERIFICATION_CODE_TTL must be positive")
	}
	if config.PendingUserTTL, err = tokenEnvDuration("AUTH_EMAIL_PENDING_USER_TTL", config.PendingUserTTL); err != nil || config.PendingUserTTL < config.CodeTTL {
		return EmailAuthConfig{}, errors.New("AUTH_EMAIL_PENDING_USER_TTL must be at least AUTH_EMAIL_VERIFICATION_CODE_TTL")
	}
	if config.SMTPUsername == "" || config.SMTPPassword == "" || config.SenderAddress == "" || config.RedisAddr == "" {
		return EmailAuthConfig{}, errors.New("AUTH_EMAIL_SMTP_USERNAME, AUTH_EMAIL_SMTP_PASSWORD, AUTH_EMAIL_SENDER_ADDRESS, and AUTH_EMAIL_REDIS_ADDR are required when AUTH_EMAIL_SMTP_HOST is set")
	}
	if _, err := mail.ParseAddress(config.SenderAddress); err != nil {
		return EmailAuthConfig{}, errors.New("AUTH_EMAIL_SENDER_ADDRESS is invalid")
	}
	if config.ImplicitTLS && config.StartTLS {
		return EmailAuthConfig{}, errors.New("AUTH_EMAIL_SMTP_STARTTLS and AUTH_EMAIL_SMTP_IMPLICIT_TLS cannot both be true")
	}
	if config.SignupIPLimit, err = emailIntEnv("AUTH_EMAIL_SIGNUP_IP_LIMIT", config.SignupIPLimit); err != nil || config.SignupIPLimit < 1 {
		return EmailAuthConfig{}, errors.New("AUTH_EMAIL_SIGNUP_IP_LIMIT must be a positive integer")
	}
	if config.SignupEmailLimit, err = emailIntEnv("AUTH_EMAIL_SIGNUP_EMAIL_LIMIT", config.SignupEmailLimit); err != nil || config.SignupEmailLimit < 1 {
		return EmailAuthConfig{}, errors.New("AUTH_EMAIL_SIGNUP_EMAIL_LIMIT must be a positive integer")
	}
	if config.SignupWindow, err = tokenEnvDuration("AUTH_EMAIL_SIGNUP_WINDOW", config.SignupWindow); err != nil || config.SignupWindow <= 0 {
		return EmailAuthConfig{}, errors.New("AUTH_EMAIL_SIGNUP_WINDOW must be positive")
	}
	if config.SignupResendInterval, err = tokenEnvDuration("AUTH_EMAIL_SIGNUP_RESEND_INTERVAL", config.SignupResendInterval); err != nil || config.SignupResendInterval <= 0 {
		return EmailAuthConfig{}, errors.New("AUTH_EMAIL_SIGNUP_RESEND_INTERVAL must be positive")
	}
	if config.CodeMaxAttempts, err = emailIntEnv("AUTH_EMAIL_CODE_MAX_ATTEMPTS", config.CodeMaxAttempts); err != nil || config.CodeMaxAttempts < 1 {
		return EmailAuthConfig{}, errors.New("AUTH_EMAIL_CODE_MAX_ATTEMPTS must be a positive integer")
	}
	if config.LoginMaxFailures, err = emailIntEnv("AUTH_EMAIL_LOGIN_MAX_FAILURES", config.LoginMaxFailures); err != nil || config.LoginMaxFailures < 1 {
		return EmailAuthConfig{}, errors.New("AUTH_EMAIL_LOGIN_MAX_FAILURES must be a positive integer")
	}
	if config.LoginFailureWindow, err = tokenEnvDuration("AUTH_EMAIL_LOGIN_FAILURE_WINDOW", config.LoginFailureWindow); err != nil || config.LoginFailureWindow <= 0 {
		return EmailAuthConfig{}, errors.New("AUTH_EMAIL_LOGIN_FAILURE_WINDOW must be positive")
	}
	if config.LoginFailureDelay, err = tokenEnvDuration("AUTH_EMAIL_LOGIN_FAILURE_DELAY", config.LoginFailureDelay); err != nil || config.LoginFailureDelay < 0 {
		return EmailAuthConfig{}, errors.New("AUTH_EMAIL_LOGIN_FAILURE_DELAY must not be negative")
	}
	return config, nil
}

func (c EmailAuthConfig) Enabled() bool { return c.SMTPHost != "" }

func emailBoolEnv(key string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return parsed, nil
}

func emailIntEnv(key string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	return strconv.Atoi(value)
}

type VerificationEmailSender interface {
	SendVerificationCode(context.Context, string, string) error
}

type smtpVerificationEmailSender struct {
	config EmailAuthConfig
}

func NewSMTPVerificationEmailSender(config EmailAuthConfig) (VerificationEmailSender, error) {
	if !config.Enabled() {
		return nil, errors.New("email SMTP is not configured")
	}
	return &smtpVerificationEmailSender{config: config}, nil
}

func (s *smtpVerificationEmailSender) SendVerificationCode(ctx context.Context, recipient, code string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	address := fmt.Sprintf("%s:%d", s.config.SMTPHost, s.config.SMTPPort)
	var connection *smtp.Client
	var err error
	if s.config.ImplicitTLS {
		raw, dialErr := tls.Dial("tcp", address, &tls.Config{ServerName: s.config.SMTPHost, MinVersion: tls.VersionTLS12})
		if dialErr != nil {
			return fmt.Errorf("connect SMTP: %w", dialErr)
		}
		connection, err = smtp.NewClient(raw, s.config.SMTPHost)
	} else {
		connection, err = smtp.Dial(address)
	}
	if err != nil {
		return fmt.Errorf("connect SMTP: %w", err)
	}
	defer connection.Quit()

	if s.config.StartTLS {
		if ok, _ := connection.Extension("STARTTLS"); !ok {
			return errors.New("SMTP server does not support STARTTLS")
		}
		if err := connection.StartTLS(&tls.Config{ServerName: s.config.SMTPHost, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("start SMTP TLS: %w", err)
		}
	}
	if ok, _ := connection.Extension("AUTH"); ok {
		auth := smtp.PlainAuth("", s.config.SMTPUsername, s.config.SMTPPassword, s.config.SMTPHost)
		if err := connection.Auth(auth); err != nil {
			return fmt.Errorf("authenticate SMTP: %w", err)
		}
	} else {
		return errors.New("SMTP server does not support authentication")
	}
	if err := connection.Mail(s.config.SenderAddress); err != nil {
		return err
	}
	if err := connection.Rcpt(recipient); err != nil {
		return err
	}
	writer, err := connection.Data()
	if err != nil {
		return err
	}

	from := s.config.SenderAddress
	if s.config.SenderName != "" {
		from = (&mail.Address{Name: s.config.SenderName, Address: s.config.SenderAddress}).String()
	}
	message := "To: " + recipient + "\r\n" +
		"From: " + from + "\r\n" +
		"Subject: InnoLive 이메일 인증 코드\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n\r\n" +
		"InnoLive 회원가입 인증 코드입니다.\r\n\r\n" + code + "\r\n"
	if _, err := writer.Write([]byte(message)); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}

// PendingEmailSignup is stored in Redis, never in PostgreSQL. The credentials
// are bcrypt hashes and the key expires automatically after PendingUserTTL.
type PendingEmailSignup struct {
	Email        string `json:"email"`
	PasswordHash string `json:"password_hash"`
}

type PendingEmailSignupStore interface {
	Save(context.Context, string, PendingEmailSignup, string, time.Duration, time.Duration) error
	PendingUser(context.Context, string) (PendingEmailSignup, error)
	VerificationCodeHash(context.Context, string) (string, error)
	ConsumeVerificationCode(context.Context, string) (string, error)
	Delete(context.Context, string) error
	Close() error
}

type redisPendingEmailSignupStore struct {
	client *redis.Client
}

func NewRedisPendingEmailSignupStore(ctx context.Context, config EmailAuthConfig) (PendingEmailSignupStore, error) {
	if !config.Enabled() || config.RedisAddr == "" {
		return nil, errors.New("email Redis is not configured")
	}
	client := redis.NewClient(&redis.Options{
		Addr:     config.RedisAddr,
		Username: config.RedisUsername,
		Password: config.RedisPassword,
		DB:       config.RedisDB,
	})
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect email Redis: %w", err)
	}
	return &redisPendingEmailSignupStore{client: client}, nil
}

func (s *redisPendingEmailSignupStore) Save(ctx context.Context, token string, pending PendingEmailSignup, codeHash string, pendingTTL, codeTTL time.Duration) error {
	data, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	pipe := s.client.TxPipeline()
	pipe.Set(ctx, pendingUserKey(token), data, pendingTTL)
	pipe.Set(ctx, verificationCodeKey(token), codeHash, codeTTL)
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisPendingEmailSignupStore) PendingUser(ctx context.Context, token string) (PendingEmailSignup, error) {
	data, err := s.client.Get(ctx, pendingUserKey(token)).Bytes()
	if errors.Is(err, redis.Nil) {
		return PendingEmailSignup{}, ErrEmailVerificationInvalid
	}
	if err != nil {
		return PendingEmailSignup{}, err
	}
	var pending PendingEmailSignup
	if err := json.Unmarshal(data, &pending); err != nil {
		return PendingEmailSignup{}, fmt.Errorf("decode pending email signup: %w", err)
	}
	return pending, nil
}

func (s *redisPendingEmailSignupStore) VerificationCodeHash(ctx context.Context, token string) (string, error) {
	value, err := s.client.Get(ctx, verificationCodeKey(token)).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrEmailVerificationInvalid
	}
	return value, err
}

func (s *redisPendingEmailSignupStore) ConsumeVerificationCode(ctx context.Context, token string) (string, error) {
	value, err := s.client.GetDel(ctx, verificationCodeKey(token)).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrEmailVerificationInvalid
	}
	return value, err
}

func (s *redisPendingEmailSignupStore) Delete(ctx context.Context, token string) error {
	return s.client.Del(ctx, pendingUserKey(token), verificationCodeKey(token)).Err()
}

func (s *redisPendingEmailSignupStore) Close() error { return s.client.Close() }

func pendingUserKey(token string) string      { return pendingUserKeyPrefix + token }
func verificationCodeKey(token string) string { return verificationCodeKeyPrefix + token }

// EmailRateLimiter provides the atomic counters that gate email authentication
// against abuse. Keys carry a TTL so limits reset without a background sweeper.
type EmailRateLimiter interface {
	// Increment adds one to a counter, sets its TTL on first creation, and
	// returns the new value.
	Increment(ctx context.Context, key string, ttl time.Duration) (int64, error)
	// Count returns the current counter value, or zero when the key is absent.
	Count(ctx context.Context, key string) (int64, error)
	// SetIfAbsent creates the key with the given TTL only when it does not
	// already exist, reporting whether it was created.
	SetIfAbsent(ctx context.Context, key string, ttl time.Duration) (bool, error)
	// ClearKey removes a counter.
	ClearKey(ctx context.Context, key string) error
}

func (s *redisPendingEmailSignupStore) Increment(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	value, err := s.client.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if value == 1 {
		if err := s.client.Expire(ctx, key, ttl).Err(); err != nil {
			return 0, err
		}
	}
	return value, nil
}

func (s *redisPendingEmailSignupStore) Count(ctx context.Context, key string) (int64, error) {
	value, err := s.client.Get(ctx, key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return value, err
}

func (s *redisPendingEmailSignupStore) SetIfAbsent(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return s.client.SetNX(ctx, key, 1, ttl).Result()
}

func (s *redisPendingEmailSignupStore) ClearKey(ctx context.Context, key string) error {
	return s.client.Del(ctx, key).Err()
}

func signupIPRateKey(ip string) string       { return signupIPRateKeyPrefix + ip }
func signupEmailRateKey(email string) string { return signupEmailRateKeyPrefix + email }
func signupResendKey(email string) string    { return signupResendKeyPrefix + email }
func codeAttemptKey(token string) string     { return codeAttemptKeyPrefix + token }
func loginFailureKey(email string) string    { return loginFailureKeyPrefix + email }

type EmailAccountStore interface {
	EmailAlreadyRegistered(context.Context, string) (bool, error)
	CreateEmailUser(context.Context, PendingEmailSignup, time.Time) (uuid.UUID, error)
	FindEmailAccount(context.Context, string) (EmailAccount, User, error)
}

type gormEmailAccountStore struct {
	db *gorm.DB
}

func NewGormEmailAccountStore(db *gorm.DB) EmailAccountStore {
	return &gormEmailAccountStore{db: db}
}

func (s *gormEmailAccountStore) EmailAlreadyRegistered(ctx context.Context, email string) (bool, error) {
	if s == nil || s.db == nil {
		return false, ErrEmailDeliveryUnavailable
	}
	var count int64
	err := s.db.WithContext(ctx).Model(&User{}).Where("LOWER(email) = ? AND status = ?", email, UserStatusActive).Count(&count).Error
	return count > 0, err
}

func (s *gormEmailAccountStore) CreateEmailUser(ctx context.Context, pending PendingEmailSignup, now time.Time) (uuid.UUID, error) {
	if s == nil || s.db == nil {
		return uuid.Nil, ErrEmailDeliveryUnavailable
	}

	var userID uuid.UUID
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&User{}).Where("LOWER(email) = ? AND status = ?", pending.Email, UserStatusActive).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return ErrEmailAlreadyRegistered
		}
		user := User{ID: uuid.New(), Email: &pending.Email, Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		account := EmailAccount{UserID: user.ID, Email: pending.Email, PasswordHash: pending.PasswordHash, CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&account).Error; err != nil {
			return err
		}
		userID = user.ID
		return nil
	})
	return userID, err
}

func (s *gormEmailAccountStore) FindEmailAccount(ctx context.Context, email string) (EmailAccount, User, error) {
	if s == nil || s.db == nil {
		return EmailAccount{}, User{}, ErrEmailDeliveryUnavailable
	}
	var account EmailAccount
	result := s.db.WithContext(ctx).Preload("User").Where("email = ?", email).Take(&account)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return EmailAccount{}, User{}, ErrEmailCredentialsInvalid
	}
	if result.Error != nil {
		return EmailAccount{}, User{}, result.Error
	}
	if account.User == nil {
		return EmailAccount{}, User{}, errors.New("email account has no user")
	}
	return account, *account.User, nil
}

type EmailAuthService struct {
	pending  PendingEmailSignupStore
	accounts EmailAccountStore
	sender   VerificationEmailSender
	tokens   *TokenService
	limiter  EmailRateLimiter
	config   EmailAuthConfig
	now      func() time.Time
}

func NewEmailAuthService(pending PendingEmailSignupStore, accounts EmailAccountStore, sender VerificationEmailSender, tokens *TokenService, limiter EmailRateLimiter, config EmailAuthConfig) (*EmailAuthService, error) {
	if pending == nil || accounts == nil || sender == nil || tokens == nil || limiter == nil || !config.Enabled() {
		return nil, errors.New("email authentication dependencies must be configured")
	}
	return &EmailAuthService{pending: pending, accounts: accounts, sender: sender, tokens: tokens, limiter: limiter, config: config, now: func() time.Time { return time.Now().UTC() }}, nil
}

// StartSignup matches Clash's signup contract: generate a signup token, cache
// the pending user for 30 minutes and code for 5 minutes, then send the code.
func (s *EmailAuthService) StartSignup(ctx context.Context, email, password, clientIP string) (string, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrEmailSignupInvalid, err)
	}
	if err := validatePassword(password); err != nil {
		return "", fmt.Errorf("%w: %v", ErrEmailSignupInvalid, err)
	}
	alreadyRegistered, err := s.accounts.EmailAlreadyRegistered(ctx, email)
	if err != nil {
		return "", err
	}
	if alreadyRegistered {
		return "", ErrEmailAlreadyRegistered
	}
	// Throttle before the expensive bcrypt hashing and SMTP delivery so a caller
	// cannot bomb an address or burn CPU with repeated requests.
	if err := s.throttleSignup(ctx, clientIP, email); err != nil {
		return "", err
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), s.config.BcryptCost)
	if err != nil {
		return "", err
	}
	code, err := newEmailVerificationCode()
	if err != nil {
		return "", err
	}
	codeHash, err := bcrypt.GenerateFromPassword([]byte(code), s.config.BcryptCost)
	if err != nil {
		return "", err
	}
	token, err := newSignupToken()
	if err != nil {
		return "", err
	}
	pending := PendingEmailSignup{Email: email, PasswordHash: string(passwordHash)}
	if err := s.pending.Save(ctx, token, pending, string(codeHash), s.config.PendingUserTTL, s.config.CodeTTL); err != nil {
		return "", fmt.Errorf("save pending email signup: %w", err)
	}
	if err := s.sender.SendVerificationCode(ctx, email, code); err != nil {
		_ = s.pending.Delete(ctx, token)
		return "", fmt.Errorf("send verification email: %w", err)
	}
	return token, nil
}

// throttleSignup applies the resend interval and the per-email and per-IP
// request caps. Non-fresh resend or an exceeded cap returns ErrEmailSignupThrottled.
func (s *EmailAuthService) throttleSignup(ctx context.Context, clientIP, email string) error {
	fresh, err := s.limiter.SetIfAbsent(ctx, signupResendKey(email), s.config.SignupResendInterval)
	if err != nil {
		return err
	}
	if !fresh {
		return ErrEmailSignupThrottled
	}
	emailCount, err := s.limiter.Increment(ctx, signupEmailRateKey(email), s.config.SignupWindow)
	if err != nil {
		return err
	}
	if emailCount > int64(s.config.SignupEmailLimit) {
		return ErrEmailSignupThrottled
	}
	if clientIP != "" {
		ipCount, err := s.limiter.Increment(ctx, signupIPRateKey(clientIP), s.config.SignupWindow)
		if err != nil {
			return err
		}
		if ipCount > int64(s.config.SignupIPLimit) {
			return ErrEmailSignupThrottled
		}
	}
	return nil
}

// CompleteSignup validates but does not issue a token. Like Clash, the client
// signs in through the separate sign-in endpoint after email verification.
func (s *EmailAuthService) CompleteSignup(ctx context.Context, signupToken, code string) error {
	if !validEmailVerificationCode(code) || strings.TrimSpace(signupToken) == "" {
		return ErrEmailVerificationInvalid
	}
	// Count the attempt before comparing so a caller cannot brute-force the
	// six-digit code. Once the cap is exceeded the code is discarded, so even a
	// correct guess afterwards fails.
	attempts, err := s.limiter.Increment(ctx, codeAttemptKey(signupToken), s.config.CodeTTL)
	if err != nil {
		return err
	}
	if attempts > int64(s.config.CodeMaxAttempts) {
		_ = s.pending.Delete(ctx, signupToken)
		return ErrEmailVerificationInvalid
	}
	codeHash, err := s.pending.VerificationCodeHash(ctx, signupToken)
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(codeHash), []byte(code)) != nil {
		return ErrEmailVerificationInvalid
	}
	// GETDEL is atomic: exactly one concurrent request can continue after a
	// correct code. We deliberately consume it before the PostgreSQL write.
	if _, err := s.pending.ConsumeVerificationCode(ctx, signupToken); err != nil {
		return err
	}
	pending, err := s.pending.PendingUser(ctx, signupToken)
	if err != nil {
		return err
	}
	if _, err := s.accounts.CreateEmailUser(ctx, pending, s.now().UTC()); err != nil {
		return err
	}
	_ = s.limiter.ClearKey(ctx, codeAttemptKey(signupToken))
	return s.pending.Delete(ctx, signupToken)
}

func (s *EmailAuthService) Login(ctx context.Context, email, password string, client ClientInfo) (TokenPair, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return TokenPair{}, ErrEmailCredentialsInvalid
	}
	failureKey := loginFailureKey(email)
	failures, err := s.limiter.Count(ctx, failureKey)
	if err != nil {
		return TokenPair{}, err
	}
	if failures >= int64(s.config.LoginMaxFailures) {
		return TokenPair{}, ErrEmailLoginThrottled
	}
	// Progressive delay grows with recent failures so brute forcing a password
	// becomes slower with every miss, without locking the account outright.
	if delay := loginFailureDelay(failures, s.config.LoginFailureDelay); delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return TokenPair{}, ctx.Err()
		}
	}
	account, user, err := s.accounts.FindEmailAccount(ctx, email)
	if err != nil {
		if errors.Is(err, ErrEmailCredentialsInvalid) {
			_, _ = s.limiter.Increment(ctx, failureKey, s.config.LoginFailureWindow)
		}
		return TokenPair{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(account.PasswordHash), []byte(password)) != nil || user.Status != UserStatusActive {
		_, _ = s.limiter.Increment(ctx, failureKey, s.config.LoginFailureWindow)
		return TokenPair{}, ErrEmailCredentialsInvalid
	}
	_ = s.limiter.ClearKey(ctx, failureKey)
	return s.tokens.IssuePair(ctx, user.ID, client)
}

func loginFailureDelay(failures int64, base time.Duration) time.Duration {
	if base <= 0 || failures <= 0 {
		return 0
	}
	delay := base * time.Duration(failures)
	if delay > emailLoginMaxDelay {
		return emailLoginMaxDelay
	}
	return delay
}

func (s *EmailAuthService) Close() error { return s.pending.Close() }

func normalizeEmail(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(strings.ToValidUTF8(value, "")))
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address != value || utf8.RuneCountInString(value) > 320 {
		return "", errors.New("invalid email")
	}
	return value, nil
}

func validatePassword(value string) error {
	if len(value) < 8 || len(value) > 72 {
		return errors.New("password must be 8 to 72 bytes")
	}
	return nil
}

func newEmailVerificationCode() (string, error) {
	ceiling := new(big.Int).Exp(big.NewInt(10), big.NewInt(emailVerificationCodeDigits), nil)
	number, err := rand.Int(rand.Reader, ceiling)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", emailVerificationCodeDigits, number.Int64()), nil
}

func newSignupToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func validEmailVerificationCode(value string) bool {
	if len(value) != emailVerificationCodeDigits {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
