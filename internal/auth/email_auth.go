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
	"unicode"
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

// EmailAuthConfig는 Clash가 쓰는 짧은 수명의 가입 계약을 따른다. 인증 코드는 5분,
// 가입 대기 사용자는 30분 유지된다. AUTH_EMAIL_SMTP_HOST가 비어 있으면 이메일
// 인증을 끈다.
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
	// SendVerificationCode는 용도(가입·계정 설정·비밀번호 변경)에 맞는 인증 코드 메일을 보낸다.
	SendVerificationCode(ctx context.Context, recipient, code string, purpose EmailPurpose) error
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

func (s *smtpVerificationEmailSender) SendVerificationCode(ctx context.Context, recipient, code string, purpose EmailPurpose) error {
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
	message, err := buildVerificationEmail(from, recipient, code, purpose, s.config.CodeTTL)
	if err != nil {
		_ = writer.Close()
		return err
	}
	if _, err := writer.Write(message); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}

// PendingEmailSignup은 PostgreSQL이 아니라 Redis에만 저장한다. 자격 정보는 bcrypt
// 해시이고, 키는 PendingUserTTL이 지나면 자동으로 만료된다.
type PendingEmailSignup struct {
	Email        string `json:"email"`
	PasswordHash string `json:"password_hash"`
	// UserID가 있으면 새 가입이 아니라 기존 OAuth 전용 사용자에 이메일 계정을 붙이는
	// 설정이다(#380).
	UserID string `json:"user_id,omitempty"`
	// Name은 가입 v2가 받는 표시 이름이다(#386). v1 가입은 비어 있다.
	Name string `json:"name,omitempty"`
	// Purpose는 이 대기 상태의 용도다(#388). 비어 있으면 가입·계정 설정(UserID 유무로 구분)이고,
	// password_reset이면 비밀번호 변경이다. 용도가 다른 확인 엔드포인트에서는 거절한다.
	Purpose EmailPurpose `json:"purpose,omitempty"`
}

func (p PendingEmailSignup) mailPurpose() EmailPurpose {
	switch {
	case p.Purpose != "":
		return p.Purpose
	case p.UserID != "":
		return EmailPurposeAccountSetup
	default:
		return EmailPurposeSignup
	}
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

// EmailRateLimiter는 이메일 인증 남용을 막는 원자적 카운터를 제공한다. 키에 TTL을
// 둬서 별도 정리 작업 없이 한도가 초기화된다.
type EmailRateLimiter interface {
	// Increment는 카운터를 1 올리고, 처음 만들 때 TTL을 걸며, 새 값을 돌려준다.
	Increment(ctx context.Context, key string, ttl time.Duration) (int64, error)
	// Count는 현재 카운터 값이다. 키가 없으면 0이다.
	Count(ctx context.Context, key string) (int64, error)
	// SetIfAbsent는 키가 없을 때만 주어진 TTL로 만들고, 만들었는지를 돌려준다.
	SetIfAbsent(ctx context.Context, key string, ttl time.Duration) (bool, error)
	// ClearKey는 카운터를 지운다.
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
	// 아래 둘은 기존 OAuth 전용 사용자의 이메일 계정 설정용이다(#380).
	EmailRegisteredToOther(context.Context, string, uuid.UUID) (bool, error)
	AttachEmailAccount(context.Context, uuid.UUID, PendingEmailSignup, time.Time) error
	// ResetPassword는 비밀번호를 바꾸고 그 사용자의 refresh 세션을 모두 폐기한다(#388).
	ResetPassword(ctx context.Context, userID uuid.UUID, email, passwordHash string, now time.Time) error
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
		user := User{ID: uuid.New(), Email: &pending.Email, DisplayName: googleOptionalString(pending.Name, 100), Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
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

// EmailRegisteredToOther는 email이 userID가 아닌 사용자의 이메일 계정이거나 활성
// 사용자 이메일인지 본다. 설정 중인 사용자 자신의 기존 이메일은 충돌로 보지 않는다.
func (s *gormEmailAccountStore) EmailRegisteredToOther(ctx context.Context, email string, userID uuid.UUID) (bool, error) {
	if s == nil || s.db == nil {
		return false, ErrEmailDeliveryUnavailable
	}
	return emailRegisteredToOther(s.db.WithContext(ctx), email, userID)
}

func emailRegisteredToOther(db *gorm.DB, email string, userID uuid.UUID) (bool, error) {
	var count int64
	if err := db.Model(&EmailAccount{}).Where("email = ? AND user_id <> ?", email, userID).Count(&count).Error; err != nil {
		return false, err
	}
	if count != 0 {
		return true, nil
	}
	err := db.Model(&User{}).Where("LOWER(email) = ? AND status = ? AND id <> ?", email, UserStatusActive, userID).Count(&count).Error
	return count != 0, err
}

// AttachEmailAccount는 기존 사용자에 이메일 계정을 만들고 users.email을 그 이메일로
// 맞춘다. 플랜·연결 계정·사용 기록은 사용자 행에 그대로 남는다.
func (s *gormEmailAccountStore) AttachEmailAccount(ctx context.Context, userID uuid.UUID, pending PendingEmailSignup, now time.Time) error {
	if s == nil || s.db == nil {
		return ErrEmailDeliveryUnavailable
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockActiveUser(tx, userID); err != nil {
			return err
		}
		hasEmail, err := userHasEmailAccount(tx, userID)
		if err != nil {
			return err
		}
		if hasEmail {
			return ErrEmailAlreadyRegistered
		}
		taken, err := emailRegisteredToOther(tx, pending.Email, userID)
		if err != nil {
			return err
		}
		if taken {
			return ErrEmailAlreadyRegistered
		}
		account := EmailAccount{UserID: userID, Email: pending.Email, PasswordHash: pending.PasswordHash, CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&account).Error; err != nil {
			return err
		}
		return tx.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{"email": pending.Email, "updated_at": now}).Error
	})
}

func (s *gormEmailAccountStore) ResetPassword(ctx context.Context, userID uuid.UUID, email, passwordHash string, now time.Time) error {
	if s == nil || s.db == nil {
		return ErrEmailDeliveryUnavailable
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockActiveUser(tx, userID); err != nil {
			return err
		}
		updated := tx.Model(&EmailAccount{}).Where("user_id = ? AND email = ?", userID, email).Updates(map[string]any{"password_hash": passwordHash, "updated_at": now})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected == 0 {
			// 코드를 받는 사이 이메일 계정이 바뀌었거나 사라졌다.
			return ErrEmailVerificationInvalid
		}
		return tx.Model(&RefreshSession{}).Where("user_id = ? AND revoked_at IS NULL", userID).
			Updates(map[string]any{"revoked_at": now, "revoke_reason": "password_changed"}).Error
	})
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

// StartSignup은 Clash의 가입 계약을 따른다. 가입 토큰을 만들고, 대기 사용자는 30분,
// 코드는 5분 캐시한 뒤 코드를 보낸다.
func (s *EmailAuthService) StartSignup(ctx context.Context, email, password, clientIP string) (string, error) {
	return s.startSignup(ctx, email, password, "", clientIP)
}

// StartSignupV2는 이름을 필수로 받는 가입 v2다(#386). 이름은 가입 완료 때 계정의
// 표시 이름으로 저장된다.
func (s *EmailAuthService) StartSignupV2(ctx context.Context, email, password, name, clientIP string) (string, error) {
	name, err := normalizeDisplayName(name)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrEmailSignupInvalid, err)
	}
	return s.startSignup(ctx, email, password, name, clientIP)
}

func (s *EmailAuthService) startSignup(ctx context.Context, email, password, name, clientIP string) (string, error) {
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
	return s.sendVerification(ctx, password, clientIP, PendingEmailSignup{Email: email, Name: name})
}

// StartAccountSetup은 #380 이전 OAuth 전용 사용자가 이메일 계정을 만드는 절차를
// 시작한다. 로그인이 내준 setup 토큰으로 사용자를 확인하고, 가입과 같은 인증 코드
// 절차를 거친다. 확인은 기존 verify-email 엔드포인트가 이어 받는다.
//
// 이메일이 이미 다른 InnoLive 계정이면 ErrEmailAlreadyRegistered다 — 그 계정으로
// 로그인해 OAuth를 연결하면 이 사용자가 그 계정으로 합쳐진다.
func (s *EmailAuthService) StartAccountSetup(ctx context.Context, setupToken, email, password, clientIP string) (string, error) {
	userID, err := s.tokens.ValidateAccountSetupToken(strings.TrimSpace(setupToken))
	if err != nil {
		return "", err
	}
	email, err = normalizeEmail(email)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrEmailSignupInvalid, err)
	}
	if err := validatePassword(password); err != nil {
		return "", fmt.Errorf("%w: %v", ErrEmailSignupInvalid, err)
	}
	taken, err := s.accounts.EmailRegisteredToOther(ctx, email, userID)
	if err != nil {
		return "", err
	}
	if taken {
		return "", ErrEmailAlreadyRegistered
	}
	return s.sendVerification(ctx, password, clientIP, PendingEmailSignup{Email: email, UserID: userID.String()})
}

// sendVerification은 가입과 계정 설정이 함께 쓰는 인증 코드 발송이다.
// pending에는 이메일·용도 등을 채워 넘기고, 비밀번호 해시는 여기서 채운다.
func (s *EmailAuthService) sendVerification(ctx context.Context, password, clientIP string, pending PendingEmailSignup) (string, error) {
	email := pending.Email
	// 비싼 bcrypt 해시와 SMTP 발송 전에 제한을 걸어, 반복 요청으로 한 주소에 폭탄을
	// 보내거나 CPU를 태우지 못하게 한다.
	throttleKey := email
	if pending.Purpose == EmailPurposePasswordReset {
		throttleKey = string(EmailPurposePasswordReset) + ":" + email
	}
	if err := s.throttleEmailCode(ctx, clientIP, throttleKey); err != nil {
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
	pending.PasswordHash = string(passwordHash)
	if err := s.pending.Save(ctx, token, pending, string(codeHash), s.config.PendingUserTTL, s.config.CodeTTL); err != nil {
		return "", fmt.Errorf("save pending email signup: %w", err)
	}
	if err := s.sender.SendVerificationCode(ctx, email, code, pending.mailPurpose()); err != nil {
		_ = s.pending.Delete(ctx, token)
		return "", fmt.Errorf("send verification email: %w", err)
	}
	return token, nil
}

// throttleEmailCode는 재발송 간격과 이메일별·IP별 요청 상한을 적용한다. 재발송이
// 너무 이르거나 상한을 넘으면 ErrEmailSignupThrottled다.
//
// 비밀번호 변경은 가입과 이메일 한도를 나눈다(#388). 가입 직후 1분 안에 비밀번호를
// 바꾸려 해도 막히지 않게 호출자가 key 앞에 용도를 붙인다.
func (s *EmailAuthService) throttleEmailCode(ctx context.Context, clientIP, email string) error {
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

// CompleteSignup은 검증만 하고 토큰을 발급하지 않는다. Clash처럼 클라이언트는 이메일
// 인증 뒤 별도 로그인 엔드포인트로 로그인한다.
func (s *EmailAuthService) CompleteSignup(ctx context.Context, signupToken, code string) error {
	pending, err := s.consumeVerification(ctx, signupToken, code, false)
	if err != nil {
		return err
	}
	if pending.UserID != "" {
		userID, err := uuid.Parse(pending.UserID)
		if err != nil {
			return fmt.Errorf("decode account setup user: %w", err)
		}
		if err := s.accounts.AttachEmailAccount(ctx, userID, pending, s.now().UTC()); err != nil {
			return err
		}
	} else if _, err := s.accounts.CreateEmailUser(ctx, pending, s.now().UTC()); err != nil {
		return err
	}
	_ = s.limiter.ClearKey(ctx, codeAttemptKey(signupToken))
	return s.pending.Delete(ctx, signupToken)
}

// consumeVerification은 코드를 확인하고 소비한 뒤 대기 상태를 돌려준다. passwordReset은
// 이 토큰이 비밀번호 변경용이어야 하는지다 — 용도가 다르면 코드를 소비하기 전에 거절해
// 가입 확인과 비밀번호 변경 토큰을 서로 쓰지 못하게 한다(#388).
func (s *EmailAuthService) consumeVerification(ctx context.Context, token, code string, passwordReset bool) (PendingEmailSignup, error) {
	if !validEmailVerificationCode(code) || strings.TrimSpace(token) == "" {
		return PendingEmailSignup{}, ErrEmailVerificationInvalid
	}
	// 비교하기 전에 시도를 세어 여섯 자리 코드를 무차별 대입하지 못하게 한다. 상한을
	// 넘으면 코드를 버리므로 그 뒤에는 맞게 추측해도 실패한다.
	attempts, err := s.limiter.Increment(ctx, codeAttemptKey(token), s.config.CodeTTL)
	if err != nil {
		return PendingEmailSignup{}, err
	}
	if attempts > int64(s.config.CodeMaxAttempts) {
		_ = s.pending.Delete(ctx, token)
		return PendingEmailSignup{}, ErrEmailVerificationInvalid
	}
	pending, err := s.pending.PendingUser(ctx, token)
	if err != nil {
		return PendingEmailSignup{}, err
	}
	if (pending.Purpose == EmailPurposePasswordReset) != passwordReset {
		return PendingEmailSignup{}, ErrEmailVerificationInvalid
	}
	codeHash, err := s.pending.VerificationCodeHash(ctx, token)
	if err != nil {
		return PendingEmailSignup{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(codeHash), []byte(code)) != nil {
		return PendingEmailSignup{}, ErrEmailVerificationInvalid
	}
	// GETDEL은 원자적이라 맞는 코드 뒤에 진행할 수 있는 동시 요청은 정확히 하나다.
	// PostgreSQL 쓰기 전에 일부러 먼저 소비한다.
	if _, err := s.pending.ConsumeVerificationCode(ctx, token); err != nil {
		return PendingEmailSignup{}, err
	}
	return pending, nil
}

// StartPasswordReset은 로그인 여부와 관계없이 이메일 인증으로 비밀번호를 바꾸는 절차를
// 시작한다(#388). 새 비밀번호를 미리 받아 해시만 대기 상태에 둔다. 가입 여부를 드러내지
// 않도록 이메일 계정이 없어도 같은 모양의 토큰을 돌려주되 메일은 보내지 않는다.
func (s *EmailAuthService) StartPasswordReset(ctx context.Context, email, newPassword, clientIP string) (string, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrEmailSignupInvalid, err)
	}
	if err := validatePassword(newPassword); err != nil {
		return "", fmt.Errorf("%w: %v", ErrEmailSignupInvalid, err)
	}
	_, user, err := s.accounts.FindEmailAccount(ctx, email)
	if errors.Is(err, ErrEmailCredentialsInvalid) || (err == nil && user.Status != UserStatusActive) {
		if err := s.throttleEmailCode(ctx, clientIP, string(EmailPurposePasswordReset)+":"+email); err != nil {
			return "", err
		}
		return newSignupToken()
	}
	if err != nil {
		return "", err
	}
	return s.sendVerification(ctx, newPassword, clientIP, PendingEmailSignup{Email: email, UserID: user.ID.String(), Purpose: EmailPurposePasswordReset})
}

// CompletePasswordReset은 코드를 확인해 비밀번호를 바꾸고, 모든 기기의 refresh 세션을
// 끊은 뒤 요청한 기기에만 새 토큰을 준다.
func (s *EmailAuthService) CompletePasswordReset(ctx context.Context, resetToken, code string, client ClientInfo) (TokenPair, error) {
	pending, err := s.consumeVerification(ctx, resetToken, code, true)
	if err != nil {
		return TokenPair{}, err
	}
	userID, err := uuid.Parse(pending.UserID)
	if err != nil {
		return TokenPair{}, fmt.Errorf("decode password reset user: %w", err)
	}
	if err := s.accounts.ResetPassword(ctx, userID, pending.Email, pending.PasswordHash, s.now().UTC()); err != nil {
		return TokenPair{}, err
	}
	_ = s.limiter.ClearKey(ctx, codeAttemptKey(resetToken))
	_ = s.limiter.ClearKey(ctx, loginFailureKey(pending.Email))
	_ = s.pending.Delete(ctx, resetToken)
	return s.tokens.IssuePair(ctx, userID, client)
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
	// 최근 실패가 늘수록 지연을 키워, 계정을 잠그지 않으면서도 비밀번호 무차별 대입을
	// 틀릴 때마다 느리게 만든다.
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

// normalizeDisplayName은 표시 이름의 앞뒤 공백을 지우고 1~100자인지 본다. 줄바꿈 등
// 제어 문자는 받지 않는다.
func normalizeDisplayName(value string) (string, error) {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	length := utf8.RuneCountInString(value)
	if length == 0 || length > 100 {
		return "", errors.New("name must be 1 to 100 characters")
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return "", errors.New("name must not contain control characters")
		}
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
