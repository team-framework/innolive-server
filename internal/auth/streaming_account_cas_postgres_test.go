package auth

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// 이관 중 두 슬롯이 각자 토큰을 들고 있으면, 늦게 쓴 쪽이 다른 슬롯이 회전한
// 최신 토큰을 덮거나 정상 연결을 재연결 필요로 표시할 수 있다(P1-13). 교체와
// 표식은 읽은 토큰이 아직 저장돼 있을 때만 일어나야 한다.
func TestPostgresRefreshTokenUpdatesAreConditional(t *testing.T) {
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
	if err := store.Upsert(ctx, StreamingAccount{UserID: user.ID, Provider: StreamingProviderChzzk, ChannelID: "channel-1", RefreshTokenCiphertext: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	account, err := store.Get(ctx, user.ID, StreamingProviderChzzk)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.UpdateRefreshToken(ctx, account.ID, []byte{1}, []byte{2}, nil, nil); err != nil {
		t.Fatal(err)
	}
	// 옛 토큰(1)을 들고 있던 다른 슬롯의 쓰기는 거절된다.
	if err := store.UpdateRefreshToken(ctx, account.ID, []byte{1}, []byte{3}, nil, nil); !errors.Is(err, ErrRefreshTokenChanged) {
		t.Fatalf("stale rotation error = %v, want ErrRefreshTokenChanged", err)
	}
	// 옛 토큰의 거절은 표식을 남기지 않는다.
	if err := store.MarkReconnectRequired(ctx, account.ID, []byte{1}, now); err != nil {
		t.Fatal(err)
	}
	current, _ := store.Get(ctx, user.ID, StreamingProviderChzzk)
	if string(current.RefreshTokenCiphertext) != string([]byte{2}) || current.ReconnectRequiredAt != nil {
		t.Fatalf("after stale writes: token %v, reconnect %v; want token 2 and no mark", current.RefreshTokenCiphertext, current.ReconnectRequiredAt)
	}
	// 지금 토큰의 거절은 표식하고, 갱신 성공은 표식을 지운다.
	if err := store.MarkReconnectRequired(ctx, account.ID, []byte{2}, now); err != nil {
		t.Fatal(err)
	}
	if current, _ = store.Get(ctx, user.ID, StreamingProviderChzzk); current.ReconnectRequiredAt == nil {
		t.Fatal("rejection of the current token was not marked")
	}
	if err := store.ClearReconnectRequired(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	if current, _ = store.Get(ctx, user.ID, StreamingProviderChzzk); current.ReconnectRequiredAt != nil {
		t.Fatal("ClearReconnectRequired left the mark")
	}
}
