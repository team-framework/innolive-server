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

// Valid는 알려진 송출 플랫폼 식별자인지 보고한다. 값 집합은 streaming_accounts의
// chk_streaming_provider 제약과 함께 움직인다. 서버에 그 플랫폼 송출이 조립돼
// 있는지는 별개 문제다 — 그쪽은 prepare가 501로 답한다.
func (p StreamingProvider) Valid() bool {
	switch p {
	case StreamingProviderYouTube, StreamingProviderChzzk:
		return true
	}
	return false
}

var (
	ErrStreamingAccountNotFound = errors.New("streaming account not found")
	// ErrStreamingAccountSelectionRequired는 유튜브 채널이 여러 개 연결돼 있는데
	// 어느 연결로 송출할지 지정하지 않았다(#390). 서버가 대신 고르지 않는다.
	ErrStreamingAccountSelectionRequired = errors.New("streaming account must be selected")
	// ErrStreamingAccountLimit은 유튜브 채널 연결이 상한(MaxYouTubeChannels)에 닿았다.
	ErrStreamingAccountLimit = errors.New("streaming account limit reached")
)

// MaxYouTubeChannels는 계정 하나에 연결할 수 있는 유튜브 채널 수다(#390). 한 방송은
// 그중 하나로만 송출한다 — 여러 유튜브 채널 동시 송출은 하지 않는다.
const MaxYouTubeChannels = 5

type streamingAccountContextKey struct{}

// WithStreamingAccount는 이 요청이 쓸 송출 연결을 ctx에 싣는다(#390). 유튜브 연결
// 조회(Get)와 access token 발급이 이 값을 따른다. 방송 하나는 준비 때 고른 연결에
// 고정되므로, 그 방송의 플랫폼 호출은 모두 같은 연결 ID를 싣는다.
func WithStreamingAccount(ctx context.Context, accountID uuid.UUID) context.Context {
	if accountID == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, streamingAccountContextKey{}, accountID)
}

// StreamingAccountFromContext는 WithStreamingAccount로 실은 연결 ID를 돌려준다.
func StreamingAccountFromContext(ctx context.Context) (uuid.UUID, bool) {
	accountID, ok := ctx.Value(streamingAccountContextKey{}).(uuid.UUID)
	return accountID, ok && accountID != uuid.Nil
}

// StreamingAccount는 사용자가 연결한 송출 플랫폼 계정이다. 치지직은 사용자당 1개,
// 유튜브는 채널 단위로 최대 MaxYouTubeChannels개다(#390). 저장된 "선택 채널"은
// 없고, 방송을 준비할 때마다 연결 ID를 지정한다.
type StreamingAccount struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey"`

	UserID uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:uidx_streaming_user_chzzk,where:provider = 'chzzk';uniqueIndex:uidx_streaming_user_youtube_channel,priority:1,where:provider = 'youtube'"`
	User   *User     `gorm:"foreignKey:UserID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`

	Provider StreamingProvider `gorm:"type:varchar(20);not null;check:chk_streaming_provider,provider IN ('youtube','chzzk')"`

	// 연결 상태 표시("○○ 채널에 연결됨")에 쓰는 플랫폼 쪽 채널 식별 정보.
	// 연결(콜백) 시점에 플랫폼 API로 조회해 저장한다.
	ChannelID    string  `gorm:"type:varchar(255);not null;uniqueIndex:uidx_streaming_user_youtube_channel,priority:2"`
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
	// Upsert는 연결을 저장한다. 같은 연결의 재연결이면 채널 정보와 토큰을 교체하고
	// 기존 행(ID)을 유지한다. 같은 연결은 치지직은 (user, provider), 유튜브는
	// (user, channel)이다. 유튜브 새 채널이 상한을 넘으면 ErrStreamingAccountLimit.
	Upsert(ctx context.Context, account StreamingAccount) error
	// Get은 사용자의 플랫폼 연결을 돌려준다. ctx에 연결 ID(WithStreamingAccount)가
	// 있으면 그 연결을, 없으면 그 플랫폼 연결이 하나일 때 그것을 돌려준다. 유튜브
	// 연결이 여러 개인데 지정이 없으면 ErrStreamingAccountSelectionRequired.
	Get(ctx context.Context, userID uuid.UUID, provider StreamingProvider) (StreamingAccount, error)
	// ListByUser는 사용자의 모든 플랫폼 연결을 provider 순으로 돌려준다.
	ListByUser(ctx context.Context, userID uuid.UUID) ([]StreamingAccount, error)
	// UpdateRefreshToken은 토큰 갱신 응답이 새 refresh token을 담아온 경우
	// 행 락 하에 교체한다 — 한 사용자의 다중 세션이 동시에 갱신할 때 나중에
	// 실패한 쓰기가 최신 토큰을 덮지 않도록 잠근다.
	UpdateRefreshToken(ctx context.Context, id uuid.UUID, ciphertext []byte, version *int16, expiresAt *time.Time) error
	// UpdateStreamInfo는 프리로딩된 재사용 스트림 정보를 행 락 하에 저장한다.
	UpdateStreamInfo(ctx context.Context, id uuid.UUID, info StreamInfo) error
	// MarkReconnectRequired는 토큰 갱신이 무효 토큰으로 거절됐음을 기록한다.
	MarkReconnectRequired(ctx context.Context, id uuid.UUID, at time.Time) error
	// Delete는 연결 행을 삭제한다. 없으면 ErrStreamingAccountNotFound.
	Delete(ctx context.Context, id uuid.UUID) error
	// UpdateChannel은 플랫폼 쪽 채널 표시 정보를 갱신한다(사용자가 채널명을
	// 바꾼 경우의 신선도 유지 — 연결·방송 준비 시점에만 호출된다).
	UpdateChannel(ctx context.Context, id uuid.UUID, channelID string, channelTitle *string) error
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
	// 충돌 대상은 플랫폼별 부분 고유 인덱스다. PostgreSQL은 조건이 바인딩 인자면 부분
	// 인덱스를 고르지 못하므로 인덱스 정의와 같은 리터럴로 쓴다.
	conflict := clause.OnConflict{
		Columns:     []clause.Column{{Name: "user_id"}},
		TargetWhere: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "provider = 'chzzk'"}}},
	}
	if account.Provider == StreamingProviderYouTube {
		conflict = clause.OnConflict{
			Columns:     []clause.Column{{Name: "user_id"}, {Name: "channel_id"}},
			TargetWhere: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "provider = 'youtube'"}}},
		}
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockActiveUser(tx, account.UserID); err != nil {
			return err
		}
		if account.Provider == StreamingProviderYouTube {
			// 사용자 행을 잠근 뒤 세므로 동시 연결이 상한을 함께 넘지 못한다.
			var count int64
			if err := tx.Model(&StreamingAccount{}).
				Where("user_id = ? AND provider = ? AND channel_id <> ?", account.UserID, StreamingProviderYouTube, account.ChannelID).
				Count(&count).Error; err != nil {
				return err
			}
			if count >= MaxYouTubeChannels {
				return ErrStreamingAccountLimit
			}
		}
		conflict.DoUpdates = clause.AssignmentColumns(streamingUpsertColumns)
		return tx.Clauses(conflict).Create(&account).Error
	})
}

// streamingUpsertColumns는 재연결 때 덮어쓰는 컬럼이다.
//
// reconnect_required_at 포함: 재연결이 곧 재연결 필요 상태의 해소다.
// 재사용 스트림 컬럼도 포함해 초기화한다: 재연결은 채널이 바뀔 수 있으므로(치지직)
// 이전 채널에 만든 재사용 스트림을 그대로 두면 다음 Prepare가 남의 채널 스트림을
// 재사용한다. Upsert는 이 컬럼들을 항상 빈 값으로 넘기므로(연결 서비스가 채우지
// 않음) 재연결마다 리셋되고, 다음 Prepare가 새 채널에 스트림을 새로 만든다.
var streamingUpsertColumns = []string{
	"channel_id", "channel_title",
	"refresh_token_ciphertext", "token_key_version", "refresh_token_expires_at",
	"reconnect_required_at",
	"stream_id", "ingestion_address", "backup_ingestion_address",
	"rtmps_ingestion_address", "rtmps_backup_ingestion_address",
	"stream_name_ciphertext", "stream_name_key_version",
	"connected_at", "updated_at",
}

func (s *gormStreamingAccountStore) Get(ctx context.Context, userID uuid.UUID, provider StreamingProvider) (StreamingAccount, error) {
	if s == nil || s.db == nil {
		return StreamingAccount{}, errors.New("streaming account database is nil")
	}
	query := s.db.WithContext(ctx).Where("user_id = ? AND provider = ?", userID, provider)
	if accountID, ok := StreamingAccountFromContext(ctx); ok {
		query = query.Where("id = ?", accountID)
	}
	// 둘까지만 읽어 "하나뿐인지"를 가린다.
	var accounts []StreamingAccount
	if err := query.Order("created_at").Limit(2).Find(&accounts).Error; err != nil {
		return StreamingAccount{}, err
	}
	switch len(accounts) {
	case 0:
		return StreamingAccount{}, ErrStreamingAccountNotFound
	case 1:
		return accounts[0], nil
	default:
		return StreamingAccount{}, ErrStreamingAccountSelectionRequired
	}
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

func (s *gormStreamingAccountStore) UpdateChannel(ctx context.Context, id uuid.UUID, channelID string, channelTitle *string) error {
	if s == nil || s.db == nil {
		return errors.New("streaming account database is nil")
	}
	result := s.db.WithContext(ctx).Model(&StreamingAccount{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"channel_id":    channelID,
			"channel_title": channelTitle,
			"updated_at":    s.now(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrStreamingAccountNotFound
	}
	return nil
}

func (s *gormStreamingAccountStore) Delete(ctx context.Context, id uuid.UUID) error {
	if s == nil || s.db == nil {
		return errors.New("streaming account database is nil")
	}
	result := s.db.WithContext(ctx).Where("id = ?", id).Delete(&StreamingAccount{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrStreamingAccountNotFound
	}
	return nil
}

func (s *gormStreamingAccountStore) ListByUser(ctx context.Context, userID uuid.UUID) ([]StreamingAccount, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("streaming account database is nil")
	}
	var accounts []StreamingAccount
	if err := s.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("provider ASC, created_at ASC").
		Find(&accounts).Error; err != nil {
		return nil, err
	}
	return accounts, nil
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
