package auth

import (
	"time"

	"github.com/google/uuid"
)

type UserStatus string

const (
	UserStatusActive   UserStatus = "active"
	UserStatusDisabled UserStatus = "disabled"
	UserStatusDeleted  UserStatus = "deleted"
)

type OAuthProvider string

const (
	OAuthProviderGoogle OAuthProvider = "google"
	OAuthProviderApple  OAuthProvider = "apple"
)

// User 는 Google이나 Apple에 종속되지 않는 InnoLive 내부 회원이다.
type User struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`

	Email           *string `gorm:"type:varchar(320)"`
	DisplayName     *string `gorm:"type:varchar(100)"`
	ProfileImageURL *string `gorm:"type:text"`

	Status UserStatus `gorm:"type:varchar(20);not null;default:'active';index;check:chk_users_status,status IN ('active','disabled','deleted')"`

	CreatedAt time.Time `gorm:"not null"`
	UpdatedAt time.Time `gorm:"not null"`
}

func (User) TableName() string {
	return "users"
}

// OAuthAccount는 외부 로그인 공급자의 계정을 InnoLive 사용자와 연결한다.
type OAuthAccount struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`

	UserID uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:uidx_oauth_user_provider,priority:1"`

	// AutoMigrate가 users에 대한 외래 키를 생성하기 위한 association이다.
	// 조회할 때 명시적으로 Preload하지 않으면 자동 조회되지는 않는다.
	User *User `gorm:"foreignKey:UserID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`

	Provider OAuthProvider `gorm:"type:varchar(20);not null;uniqueIndex:uidx_oauth_provider_subject,priority:1;uniqueIndex:uidx_oauth_user_provider,priority:2;check:chk_oauth_provider,provider IN ('google','apple')"`

	// Google 또는 Apple ID Token의 sub 값이다.
	ProviderSubject string `gorm:"type:varchar(255);not null;uniqueIndex:uidx_oauth_provider_subject,priority:2"`

	ProviderEmail  *string `gorm:"type:varchar(320)"`
	EmailVerified  bool    `gorm:"not null;default:false"`
	IsPrivateEmail bool    `gorm:"not null;default:false"`

	// Apple 회원 탈퇴 시 Apple revoke API 호출에 필요할 수 있다.
	// 평문을 저장하지 않고 나중에 AES-GCM 등으로 암호화한 값만 저장한다.
	ProviderRefreshTokenCiphertext []byte `gorm:"type:bytea"`
	ProviderTokenKeyVersion        *int16 `gorm:"type:smallint"`

	LastLoginAt time.Time `gorm:"not null"`
	CreatedAt   time.Time `gorm:"not null"`
	UpdatedAt   time.Time `gorm:"not null"`
}

func (OAuthAccount) TableName() string {
	return "oauth_accounts"
}

// RefreshSession은 InnoLive Refresh Token 한 개에 대응한다.
type RefreshSession struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`

	UserID uuid.UUID `gorm:"type:uuid;not null;index:idx_refresh_user_expiry,priority:1"`
	User   *User     `gorm:"foreignKey:UserID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`

	// 하나의 로그인에서 파생된 Refresh Token들의 계열 ID다.
	// Rotation 중 탈취된 이전 토큰이 재사용되는 것을 탐지할 때 사용한다.
	FamilyID uuid.UUID `gorm:"type:uuid;not null;index"`

	// Refresh Token 평문이 아니라 SHA-256 결과 32바이트를 저장한다.
	TokenHash []byte `gorm:"type:bytea;not null;uniqueIndex;check:chk_refresh_token_hash_length,octet_length(token_hash) = 32"`

	ExpiresAt time.Time `gorm:"not null;index:idx_refresh_user_expiry,priority:2"`

	LastUsedAt   *time.Time
	RevokedAt    *time.Time `gorm:"index"`
	RevokeReason *string    `gorm:"type:varchar(100)"`

	// Rotation으로 대체된 새 RefreshSession의 ID다.
	ReplacedByID *uuid.UUID      `gorm:"type:uuid"`
	ReplacedBy   *RefreshSession `gorm:"foreignKey:ReplacedByID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL"`

	UserAgent *string `gorm:"type:text"`
	IPAddress *string `gorm:"type:inet"`

	CreatedAt time.Time `gorm:"not null"`
}

func (RefreshSession) TableName() string {
	return "refresh_sessions"
}
