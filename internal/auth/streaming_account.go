package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// StreamingProvider는 RTMP 송출 대상 플랫폼 식별자다. 로그인 공급자
// (OAuthProvider)와는 별개 개념이다 — 로그인은 이메일로 하고 송출은
// YouTube 채널로 하는 식으로 연결이 독립적이다.
type StreamingProvider string

const (
	StreamingProviderYouTube StreamingProvider = "youtube"
	StreamingProviderChzzk   StreamingProvider = "chzzk"
)

var ErrStreamingAccountNotFound = errors.New("streaming account not found")

// StreamingAccount는 사용자가 연결한 송출 플랫폼 계정이다. 사용자당 플랫폼별
// 1개(멀티 채널 불허)로 시작하며, 멀티 채널이 필요해지면 유니크 인덱스를 풀고
// 선택 채널 포인터를 추가하는 마이그레이션으로 확장한다.
type StreamingAccount struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`

	UserID uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:uidx_streaming_user_provider,priority:1"`
	User   *User     `gorm:"foreignKey:UserID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`

	Provider StreamingProvider `gorm:"type:varchar(20);not null;uniqueIndex:uidx_streaming_user_provider,priority:2;check:chk_streaming_provider,provider IN ('youtube','chzzk')"`

	// 연결 상태 표시("○○ 채널에 연결됨")에 쓰는 플랫폼 쪽 채널 식별 정보.
	// 연결(콜백) 시점에 플랫폼 API로 조회해 저장한다.
	ChannelID    string  `gorm:"type:varchar(255);not null"`
	ChannelTitle *string `gorm:"type:varchar(255)"`

	// 플랫폼 OAuth refresh token의 AES-GCM 암호문. 평문은 저장하지 않는다.
	RefreshTokenCiphertext []byte `gorm:"type:bytea"`
	TokenKeyVersion        *int16 `gorm:"type:smallint"`

	// refresh token 자체의 만료 시각. Google OAuth는 앱 게시 상태가 Testing이면
	// 토큰 응답에 refresh_token_expires_in(실측 7일)을 담아 보낸다 — 무기한이면
	// NULL이다. 이 값을 추적하지 않으면 연결이 만료 후 조용히 죽는다.
	RefreshTokenExpiresAt *time.Time

	// 토큰 갱신이 "무효 토큰"으로 거절된 시각. 사용자가 플랫폼 쪽에서 권한을
	// 취소하는 등 재연결 없이는 복구되지 않는 상태의 표식이며, 재연결(Upsert)
	// 시 NULL로 리셋된다. 조회 API가 "재연결 필요"를 API 호출 없이 판별하는
	// 근거다.
	ReconnectRequiredAt *time.Time

	// 치지직처럼 ingest URL을 API로 제공하지 않는 플랫폼을 위한 수동 설정값.
	// 연결(OAuth) 플로우는 이 컬럼을 건드리지 않는다.
	ManualIngestURL *string `gorm:"type:text"`

	// 이하 YouTube 재사용 스트림(isReusable=true) 프리로딩 정보 — 첫 방송
	// 준비 때 1회 생성해 저장하고 이후 방송은 재사용한다(방송당 API 3→2회).
	// 필드 구성은 liveStreams.insert 응답의 cdn.ingestionInfo 실물
	// (2026-08-10 실측: rtmp/rtmps × 주/백업 4주소 + streamName)을 따른다.
	StreamID                    *string `gorm:"type:varchar(255)"`
	IngestionAddress            *string `gorm:"type:text"`
	BackupIngestionAddress      *string `gorm:"type:text"`
	RtmpsIngestionAddress       *string `gorm:"type:text"`
	RtmpsBackupIngestionAddress *string `gorm:"type:text"`
	// streamName은 RTMP URL에 붙는 스트림 키 상당의 비밀값이라 refresh token과
	// 같은 방식(AES-GCM)으로 암호화해서만 저장한다.
	StreamNameCiphertext []byte `gorm:"type:bytea"`
	StreamNameKeyVersion *int16 `gorm:"type:smallint"`

	ConnectedAt time.Time `gorm:"not null"`
	CreatedAt   time.Time `gorm:"not null"`
	UpdatedAt   time.Time `gorm:"not null"`
}

// StreamInfo는 프리로딩된 재사용 스트림 정보의 갱신 단위다.
type StreamInfo struct {
	StreamID                    string
	IngestionAddress            string
	BackupIngestionAddress      string
	RtmpsIngestionAddress       string
	RtmpsBackupIngestionAddress string
	StreamNameCiphertext        []byte
	StreamNameKeyVersion        *int16
}

func (StreamingAccount) TableName() string { return "streaming_accounts" }

// StreamingAccountStore는 송출 계정 연결의 영속화 계약이다.
type StreamingAccountStore interface {
	// Upsert는 연결을 저장한다. 같은 (user, provider) 재연결이면 채널 정보와
	// 토큰을 교체하고 기존 행(ID)을 유지한다.
	Upsert(ctx context.Context, account StreamingAccount) error
	Get(ctx context.Context, userID uuid.UUID, provider StreamingProvider) (StreamingAccount, error)
	// UpdateRefreshToken은 토큰 갱신 응답이 새 refresh token을 담아온 경우
	// 행 락 하에 교체한다 — 한 사용자의 다중 세션이 동시에 갱신할 때 나중에
	// 실패한 쓰기가 최신 토큰을 덮지 않도록 잠근다.
	UpdateRefreshToken(ctx context.Context, id uuid.UUID, ciphertext []byte, version *int16, expiresAt *time.Time) error
	// UpdateStreamInfo는 프리로딩된 재사용 스트림 정보를 행 락 하에 저장한다.
	UpdateStreamInfo(ctx context.Context, id uuid.UUID, info StreamInfo) error
	// MarkReconnectRequired는 토큰 갱신이 무효 토큰으로 거절됐음을 기록한다.
	MarkReconnectRequired(ctx context.Context, id uuid.UUID, at time.Time) error
}

type gormStreamingAccountStore struct {
	db  *gorm.DB
	now func() time.Time
}

func NewGormStreamingAccountStore(db *gorm.DB) StreamingAccountStore {
	return &gormStreamingAccountStore{db: db, now: func() time.Time { return time.Now().UTC() }}
}

func (s *gormStreamingAccountStore) Upsert(ctx context.Context, account StreamingAccount) error {
	if s == nil || s.db == nil {
		return errors.New("streaming account database is nil")
	}
	now := s.now()
	if account.ID == uuid.Nil {
		account.ID = uuid.New()
	}
	account.ConnectedAt = now
	account.CreatedAt = now
	account.UpdatedAt = now
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureActiveUser(tx, account.UserID); err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "user_id"}, {Name: "provider"}},
			// reconnect_required_at 포함: 재연결이 곧 재연결 필요 상태의 해소다.
			DoUpdates: clause.AssignmentColumns([]string{
				"channel_id", "channel_title",
				"refresh_token_ciphertext", "token_key_version", "refresh_token_expires_at",
				"reconnect_required_at",
				"connected_at", "updated_at",
			}),
		}).Create(&account).Error
	})
}

func (s *gormStreamingAccountStore) Get(ctx context.Context, userID uuid.UUID, provider StreamingProvider) (StreamingAccount, error) {
	if s == nil || s.db == nil {
		return StreamingAccount{}, errors.New("streaming account database is nil")
	}
	var account StreamingAccount
	result := s.db.WithContext(ctx).
		Where("user_id = ? AND provider = ?", userID, provider).
		Take(&account)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return StreamingAccount{}, ErrStreamingAccountNotFound
	}
	if result.Error != nil {
		return StreamingAccount{}, result.Error
	}
	return account, nil
}

func (s *gormStreamingAccountStore) UpdateStreamInfo(ctx context.Context, id uuid.UUID, info StreamInfo) error {
	if s == nil || s.db == nil {
		return errors.New("streaming account database is nil")
	}
	now := s.now()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current StreamingAccount
		result := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", id).
			Take(&current)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return ErrStreamingAccountNotFound
		}
		if result.Error != nil {
			return result.Error
		}
		return tx.Model(&StreamingAccount{}).Where("id = ?", id).Updates(map[string]any{
			"stream_id":                      info.StreamID,
			"ingestion_address":              info.IngestionAddress,
			"backup_ingestion_address":       info.BackupIngestionAddress,
			"rtmps_ingestion_address":        info.RtmpsIngestionAddress,
			"rtmps_backup_ingestion_address": info.RtmpsBackupIngestionAddress,
			"stream_name_ciphertext":         info.StreamNameCiphertext,
			"stream_name_key_version":        info.StreamNameKeyVersion,
			"updated_at":                     now,
		}).Error
	})
}

func (s *gormStreamingAccountStore) MarkReconnectRequired(ctx context.Context, id uuid.UUID, at time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("streaming account database is nil")
	}
	result := s.db.WithContext(ctx).Model(&StreamingAccount{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"reconnect_required_at": at,
			"updated_at":            s.now(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrStreamingAccountNotFound
	}
	return nil
}

func (s *gormStreamingAccountStore) UpdateRefreshToken(ctx context.Context, id uuid.UUID, ciphertext []byte, version *int16, expiresAt *time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("streaming account database is nil")
	}
	now := s.now()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current StreamingAccount
		result := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", id).
			Take(&current)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return ErrStreamingAccountNotFound
		}
		if result.Error != nil {
			return result.Error
		}
		return tx.Model(&StreamingAccount{}).Where("id = ?", id).Updates(map[string]any{
			"refresh_token_ciphertext": ciphertext,
			"token_key_version":        version,
			"refresh_token_expires_at": expiresAt,
			"updated_at":               now,
		}).Error
	})
}
