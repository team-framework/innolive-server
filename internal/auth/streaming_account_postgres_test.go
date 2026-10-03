package auth

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// TestPostgresStreamingAccountUpsert는 메모리 대역이 할 수 없는 PostgreSQL 고유 부분을
// 검증한다. ON CONFLICT (user_id, channel_id) WHERE provider='youtube' upsert 경로, 고유 인덱스, 행 잠금 refresh
// token 갱신. TEST_DATABASE_URL을 주면 돌고, 매번 격리된 임시 스키마를 쓴다.
func TestPostgresStreamingAccountUpsert(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL streaming account integration test")
	}

	db := newPostgresStreamingTestDB(t, databaseURL)
	now := time.Now().UTC().Truncate(time.Microsecond)
	user := User{ID: uuid.New(), Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	store := NewGormStreamingAccountStore(db)
	ctx := context.Background()

	title := "Team Framework"
	first := StreamingAccount{
		UserID:                 user.ID,
		Provider:               StreamingProviderYouTube,
		ChannelID:              "UCfirst",
		ChannelTitle:           &title,
		RefreshTokenCiphertext: []byte{1, 2, 3},
	}
	if err := store.Upsert(ctx, first); err != nil {
		t.Fatal(err)
	}
	created, err := store.Get(ctx, user.ID, StreamingProviderYouTube)
	if err != nil {
		t.Fatal(err)
	}

	// 재연결: 같은 채널은 새 행이 아니라 기존 행 갱신이어야 한다(#390).
	second := StreamingAccount{
		UserID:                 user.ID,
		Provider:               StreamingProviderYouTube,
		ChannelID:              "UCfirst",
		RefreshTokenCiphertext: []byte{4, 5, 6},
	}
	if err := store.Upsert(ctx, second); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Get(ctx, user.ID, StreamingProviderYouTube)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != created.ID {
		t.Fatalf("re-connect created a new row: %s -> %s", created.ID, updated.ID)
	}
	if updated.ChannelID != "UCfirst" || updated.ChannelTitle != nil || string(updated.RefreshTokenCiphertext) != string([]byte{4, 5, 6}) {
		t.Fatalf("re-connect did not replace channel/token: %+v", updated)
	}

	// 재연결은 재사용 스트림을 초기화한다 — 다음 Prepare가 새로 만든다.
	if err := store.UpdateStreamInfo(ctx, updated.ID, StreamInfo{
		StreamID:              "stream-of-first-channel",
		RtmpsIngestionAddress: "rtmps://a.example/live2",
		StreamNameCiphertext:  []byte{9, 9, 9},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, StreamingAccount{
		UserID:                 user.ID,
		Provider:               StreamingProviderYouTube,
		ChannelID:              "UCfirst",
		RefreshTokenCiphertext: []byte{1, 1, 1},
	}); err != nil {
		t.Fatal(err)
	}
	reconnected, err := store.Get(ctx, user.ID, StreamingProviderYouTube)
	if err != nil {
		t.Fatal(err)
	}
	if reconnected.StreamID != nil || reconnected.RtmpsIngestionAddress != nil || reconnected.StreamNameCiphertext != nil {
		t.Fatalf("re-connect did not reset reusable stream: %+v", reconnected)
	}
	var count int64
	if err := db.Model(&StreamingAccount{}).Where("user_id = ?", user.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("rows for user = %d, want 1 (unique index)", count)
	}

	// 행 락 기반 refresh token 교체.
	expiresAt := now.Add(7 * 24 * time.Hour)
	version := int16(1)
	if err := store.UpdateRefreshToken(ctx, updated.ID, []byte{7, 8, 9}, &version, &expiresAt); err != nil {
		t.Fatal(err)
	}
	rotated, err := store.Get(ctx, user.ID, StreamingProviderYouTube)
	if err != nil {
		t.Fatal(err)
	}
	if string(rotated.RefreshTokenCiphertext) != string([]byte{7, 8, 9}) || rotated.RefreshTokenExpiresAt == nil {
		t.Fatalf("refresh token not rotated: %+v", rotated)
	}
	if err := store.UpdateRefreshToken(ctx, uuid.New(), []byte{0}, &version, nil); !errors.Is(err, ErrStreamingAccountNotFound) {
		t.Fatalf("unknown id error = %v, want ErrStreamingAccountNotFound", err)
	}

	// 재연결 필요 표식과 재연결(Upsert)에 의한 해소.
	if err := store.MarkReconnectRequired(ctx, updated.ID, now); err != nil {
		t.Fatal(err)
	}
	marked, err := store.Get(ctx, user.ID, StreamingProviderYouTube)
	if err != nil {
		t.Fatal(err)
	}
	if marked.ReconnectRequiredAt == nil {
		t.Fatal("ReconnectRequiredAt not persisted")
	}
	if err := store.Upsert(ctx, StreamingAccount{UserID: user.ID, Provider: StreamingProviderYouTube, ChannelID: "UCfirst"}); err != nil {
		t.Fatal(err)
	}
	cleared, err := store.Get(ctx, user.ID, StreamingProviderYouTube)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.ReconnectRequiredAt != nil {
		t.Fatalf("ReconnectRequiredAt = %v after reconnect, want nil", cleared.ReconnectRequiredAt)
	}
	if err := store.MarkReconnectRequired(ctx, uuid.New(), now); !errors.Is(err, ErrStreamingAccountNotFound) {
		t.Fatalf("unknown id mark error = %v, want ErrStreamingAccountNotFound", err)
	}

	// 목록 조회와 삭제.
	listed, err := store.ListByUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != updated.ID {
		t.Fatalf("ListByUser = %d items, want the user's single connection", len(listed))
	}
	if err := store.Delete(ctx, updated.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, user.ID, StreamingProviderYouTube); !errors.Is(err, ErrStreamingAccountNotFound) {
		t.Fatal("row must be gone after Delete")
	}
	if err := store.Delete(ctx, updated.ID); !errors.Is(err, ErrStreamingAccountNotFound) {
		t.Fatalf("double delete error = %v, want ErrStreamingAccountNotFound", err)
	}
	if err := store.Upsert(ctx, StreamingAccount{UserID: user.ID, Provider: StreamingProviderYouTube, ChannelID: "UCsecond"}); err != nil {
		t.Fatal(err)
	}

	// 비활성 사용자의 연결은 거부돼야 한다.
	disabled := User{ID: uuid.New(), Status: UserStatusDisabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&disabled).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, StreamingAccount{UserID: disabled.ID, Provider: StreamingProviderYouTube, ChannelID: "UCx"}); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("disabled user upsert error = %v, want ErrUserInactive", err)
	}
}

// 유튜브는 채널 단위로 여러 개(상한 5), 치지직은 1개다(#390). 채널이 여러 개면
// 연결 ID를 지정해야 조회되고, 지정이 없으면 서버가 대신 고르지 않는다.
func TestPostgresStreamingAccountsYouTubeChannelsPerUser(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL streaming account integration test")
	}
	db := newPostgresStreamingTestDB(t, databaseURL)
	now := time.Now().UTC()
	user := User{ID: uuid.New(), Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
	other := User{ID: uuid.New(), Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&[]User{user, other}).Error; err != nil {
		t.Fatal(err)
	}
	store := NewGormStreamingAccountStore(db)
	ctx := context.Background()
	youtube := func(userID uuid.UUID, channel string) StreamingAccount {
		return StreamingAccount{UserID: userID, Provider: StreamingProviderYouTube, ChannelID: channel, RefreshTokenCiphertext: []byte(channel)}
	}

	if err := store.Upsert(ctx, youtube(user.ID, "UC1")); err != nil {
		t.Fatal(err)
	}
	only, err := store.Get(ctx, user.ID, StreamingProviderYouTube)
	if err != nil || only.ChannelID != "UC1" {
		t.Fatalf("single channel get = %+v, %v", only, err)
	}
	if err := store.Upsert(ctx, youtube(user.ID, "UC2")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, user.ID, StreamingProviderYouTube); !errors.Is(err, ErrStreamingAccountSelectionRequired) {
		t.Fatalf("get without selection error = %v, want ErrStreamingAccountSelectionRequired", err)
	}
	listed, err := store.ListByUser(ctx, user.ID)
	if err != nil || len(listed) != 2 {
		t.Fatalf("list = %d, %v", len(listed), err)
	}
	second := listed[1]
	if second.ChannelID != "UC2" {
		t.Fatalf("list order = %s, want connection order", second.ChannelID)
	}
	picked, err := store.Get(WithStreamingAccount(ctx, second.ID), user.ID, StreamingProviderYouTube)
	if err != nil || picked.ID != second.ID {
		t.Fatalf("selected get = %+v, %v", picked, err)
	}
	// 남의 연결 ID로는 조회되지 않는다.
	if _, err := store.Get(WithStreamingAccount(ctx, second.ID), other.ID, StreamingProviderYouTube); !errors.Is(err, ErrStreamingAccountNotFound) {
		t.Fatalf("foreign account get error = %v, want ErrStreamingAccountNotFound", err)
	}

	// 같은 채널 재연결은 행을 늘리지 않고, 상한은 새 채널에만 걸린다.
	for _, channel := range []string{"UC3", "UC4", "UC5", "UC2"} {
		if err := store.Upsert(ctx, youtube(user.ID, channel)); err != nil {
			t.Fatalf("upsert %s: %v", channel, err)
		}
	}
	if err := store.Upsert(ctx, youtube(user.ID, "UC6")); !errors.Is(err, ErrStreamingAccountLimit) {
		t.Fatalf("sixth channel error = %v, want ErrStreamingAccountLimit", err)
	}
	// 다른 사용자는 같은 채널을 따로 연결할 수 있다(브랜드 채널 공동 관리).
	if err := store.Upsert(ctx, youtube(other.ID, "UC1")); err != nil {
		t.Fatalf("other user same channel: %v", err)
	}

	// 치지직은 사용자당 1개 — 다른 채널로 재연결해도 같은 행을 갱신한다.
	if err := store.Upsert(ctx, StreamingAccount{UserID: user.ID, Provider: StreamingProviderChzzk, ChannelID: "chzzk-a"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, StreamingAccount{UserID: user.ID, Provider: StreamingProviderChzzk, ChannelID: "chzzk-b"}); err != nil {
		t.Fatal(err)
	}
	chzzk, err := store.Get(ctx, user.ID, StreamingProviderChzzk)
	if err != nil || chzzk.ChannelID != "chzzk-b" {
		t.Fatalf("chzzk reconnect = %+v, %v", chzzk, err)
	}
	var count int64
	db.Model(&StreamingAccount{}).Where("user_id = ?", user.ID).Count(&count)
	if count != 6 {
		t.Fatalf("rows = %d, want 5 youtube + 1 chzzk", count)
	}
}

// 기동 시 AutoMigrate가 옛 사용자·플랫폼 고유 키를 제약·인덱스 어느 형태든 지우고
// 플랫폼별 부분 고유 인덱스로 바꾼다(#390).
func TestPostgresAutoMigrateSwapsStreamingUserProviderUnique(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL migration integration test")
	}
	for _, legacy := range []string{
		"ALTER TABLE streaming_accounts ADD CONSTRAINT uidx_streaming_user_provider UNIQUE (user_id, provider)",
		"CREATE UNIQUE INDEX uidx_streaming_user_provider ON streaming_accounts (user_id, provider)",
	} {
		db := newPostgresRefreshTestDB(t, databaseURL)
		ctx := context.Background()
		if err := AutoMigrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{"DROP INDEX uidx_streaming_user_chzzk", "DROP INDEX uidx_streaming_user_youtube_channel", legacy} {
			if err := db.Exec(statement).Error; err != nil {
				t.Fatal(err)
			}
		}
		if err := AutoMigrate(ctx, db); err != nil {
			t.Fatalf("auto migrate over %q: %v", legacy, err)
		}
		var names []string
		db.Raw("SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'streaming_accounts'").Scan(&names)
		joined := strings.Join(names, ",")
		if strings.Contains(joined, "uidx_streaming_user_provider") || !strings.Contains(joined, "uidx_streaming_user_chzzk") || !strings.Contains(joined, "uidx_streaming_user_youtube_channel") {
			t.Fatalf("indexes after %q = %s", legacy, joined)
		}

		now := time.Now().UTC()
		user := User{ID: uuid.New(), Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		insert := func(provider StreamingProvider, channel string) error {
			return db.Create(&StreamingAccount{ID: uuid.New(), UserID: user.ID, Provider: provider, ChannelID: channel, ConnectedAt: now, CreatedAt: now, UpdatedAt: now}).Error
		}
		if err := insert(StreamingProviderYouTube, "UC1"); err != nil {
			t.Fatal(err)
		}
		if err := insert(StreamingProviderYouTube, "UC2"); err != nil {
			t.Fatalf("second youtube channel rejected after %q: %v", legacy, err)
		}
		if err := insert(StreamingProviderYouTube, "UC1"); err == nil {
			t.Fatalf("database accepted the same youtube channel twice after %q", legacy)
		}
		if err := insert(StreamingProviderChzzk, "c1"); err != nil {
			t.Fatal(err)
		}
		if err := insert(StreamingProviderChzzk, "c2"); err == nil {
			t.Fatalf("database accepted a second chzzk link after %q", legacy)
		}
	}
}

func newPostgresStreamingTestDB(t *testing.T, databaseURL string) *gorm.DB {
	t.Helper()
	admin, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	schema := "auth_streaming_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Errorf("drop PostgreSQL test schema: %v", err)
		}
	})

	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&User{}, &StreamingAccount{}); err != nil {
		t.Fatal(err)
	}
	return db
}
