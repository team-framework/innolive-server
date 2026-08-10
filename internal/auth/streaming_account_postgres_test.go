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

// TestPostgresStreamingAccountUpsert verifies the PostgreSQL-specific pieces a
// memory double cannot: the ON CONFLICT (user_id, provider) upsert path, the
// unique index, and the row-locked refresh-token update. Set TEST_DATABASE_URL
// to run it; each run uses an isolated, temporary schema.
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

	// 재연결: 같은 (user, provider)는 새 행이 아니라 기존 행 갱신이어야 한다.
	second := StreamingAccount{
		UserID:                 user.ID,
		Provider:               StreamingProviderYouTube,
		ChannelID:              "UCsecond",
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
	if updated.ChannelID != "UCsecond" || string(updated.RefreshTokenCiphertext) != string([]byte{4, 5, 6}) {
		t.Fatalf("re-connect did not replace channel/token: %+v", updated)
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
	if err := store.Upsert(ctx, StreamingAccount{UserID: user.ID, Provider: StreamingProviderYouTube, ChannelID: "UCsecond"}); err != nil {
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
