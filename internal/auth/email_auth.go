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
)

var (
	ErrEmailAlreadyRegistered   = errors.New("email already registered")
	ErrEmailSignupInvalid       = errors.New("email signup is invalid")
	ErrEmailVerificationInvalid = errors.New("email verification is invalid")
	ErrEmailCredentialsInvalid  = errors.New("email credentials are invalid")
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
	config   EmailAuthConfig
	now      func() time.Time
}

func NewEmailAuthService(pending PendingEmailSignupStore, accounts EmailAccountStore, sender VerificationEmailSender, tokens *TokenService, config EmailAuthConfig) (*EmailAuthService, error) {
	if pending == nil || accounts == nil || sender == nil || tokens == nil || !config.Enabled() {
		return nil, errors.New("email authentication dependencies must be configured")
	}
	return &EmailAuthService{pending: pending, accounts: accounts, sender: sender, tokens: tokens, config: config, now: func() time.Time { return time.Now().UTC() }}, nil
}

// StartSignup matches Clash's signup contract: generate a signup token, cache
// the pending user for 30 minutes and code for 5 minutes, then send the code.
func (s *EmailAuthService) StartSignup(ctx context.Context, email, password string) (string, error) {
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

// CompleteSignup validates but does not issue a token. Like Clash, the client
// signs in through the separate sign-in endpoint after email verification.
func (s *EmailAuthService) CompleteSignup(ctx context.Context, signupToken, code string) error {
	if !validEmailVerificationCode(code) || strings.TrimSpace(signupToken) == "" {
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
	return s.pending.Delete(ctx, signupToken)
}

func (s *EmailAuthService) Login(ctx context.Context, email, password string, client ClientInfo) (TokenPair, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return TokenPair{}, ErrEmailCredentialsInvalid
	}
	account, user, err := s.accounts.FindEmailAccount(ctx, email)
	if err != nil {
		return TokenPair{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(account.PasswordHash), []byte(password)) != nil || user.Status != UserStatusActive {
		return TokenPair{}, ErrEmailCredentialsInvalid
	}
	return s.tokens.IssuePair(ctx, user.ID, client)
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
