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

func TestPostgresWithdrawalDeletesAccountData(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL account withdrawal integration test")
	}

	db := newPostgresWithdrawalTestDB(t, databaseURL)
	now := time.Now().UTC().Truncate(time.Microsecond)
	target := seedWithdrawalUser(t, db, now, "withdrawal@example.com", "withdrawal")
	other := seedWithdrawalUser(t, db, now, "other@example.com", "other")

	service, err := NewAccountWithdrawalService(NewGormWithdrawalAccountStore(db), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Withdraw(context.Background(), target.ID); err != nil {
		t.Fatal(err)
	}

	assertWithdrawalCounts(t, db, target.ID, [5]int64{})
	assertWithdrawalCounts(t, db, other.ID, [5]int64{1, 1, 2, 1, 1})
	if _, err := NewGormUserStatusChecker(db).UserStatus(context.Background(), target.ID); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("deleted user status error = %v, want ErrUserInactive", err)
	}

	// The provider subject and email become available for a new account after
	// the old user's rows have been deleted.
	reRegistered := seedWithdrawalUser(t, db, now, *target.Email, "withdrawal")
	assertWithdrawalCounts(t, db, reRegistered.ID, [5]int64{1, 1, 2, 1, 1})
}

func TestPostgresWithdrawalRollsBackPartialDelete(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL account withdrawal integration test")
	}

	db := newPostgresWithdrawalTestDB(t, databaseURL)
	now := time.Now().UTC().Truncate(time.Microsecond)
	target := seedWithdrawalUser(t, db, now, "rollback@example.com", "rollback")
	// This unrelated FK deliberately rejects the final user DELETE. The child
	// rows must be restored by the same transaction rather than left partially
	// removed.
	if err := db.Exec("CREATE TABLE withdrawal_delete_guards (user_id uuid PRIMARY KEY REFERENCES users(id))").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO withdrawal_delete_guards (user_id) VALUES (?)", target.ID).Error; err != nil {
		t.Fatal(err)
	}

	service, err := NewAccountWithdrawalService(NewGormWithdrawalAccountStore(db), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Withdraw(context.Background(), target.ID); err == nil {
		t.Fatal("withdrawal unexpectedly succeeded while the final user delete was blocked")
	}
	assertWithdrawalCounts(t, db, target.ID, [5]int64{1, 1, 2, 1, 1})

	if err := db.Exec("DELETE FROM withdrawal_delete_guards WHERE user_id = ?", target.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.Withdraw(context.Background(), target.ID); err != nil {
		t.Fatalf("withdrawal retry failed after rollback: %v", err)
	}
	assertWithdrawalCounts(t, db, target.ID, [5]int64{})
}

func seedWithdrawalUser(t *testing.T, db *gorm.DB, now time.Time, email, subjectPrefix string) User {
	t.Helper()
	user := User{ID: uuid.New(), Email: &email, Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&EmailAccount{
		UserID: user.ID, Email: email, PasswordHash: "hash", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	for _, provider := range []OAuthProvider{OAuthProviderGoogle, OAuthProviderApple} {
		if err := db.Create(&OAuthAccount{
			ID: uuid.New(), UserID: user.ID, Provider: provider,
			ProviderSubject: subjectPrefix + "-" + string(provider), LastLoginAt: now,
			CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	tokenHash := make([]byte, 32)
	tokenSeed := uuid.New()
	copy(tokenHash, tokenSeed[:])
	copy(tokenHash[len(tokenSeed):], tokenSeed[:])
	if err := db.Create(&RefreshSession{
		ID: uuid.New(), UserID: user.ID, FamilyID: uuid.New(), TokenHash: tokenHash,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&StreamingAccount{
		ID: uuid.New(), UserID: user.ID, Provider: StreamingProviderYouTube,
		ChannelID: "UC-" + subjectPrefix, ConnectedAt: now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return user
}

func assertWithdrawalCounts(t *testing.T, db *gorm.DB, userID uuid.UUID, want [5]int64) {
	t.Helper()
	for index, check := range []struct {
		table, column string
	}{
		{table: "users", column: "id"},
		{table: "email_accounts", column: "user_id"},
		{table: "oauth_accounts", column: "user_id"},
		{table: "refresh_sessions", column: "user_id"},
		{table: "streaming_accounts", column: "user_id"},
	} {
		var count int64
		if err := db.Table(check.table).Where(check.column+" = ?", userID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != want[index] {
			t.Errorf("%s rows for %s = %d, want %d", check.table, userID, count, want[index])
		}
	}
}

func newPostgresWithdrawalTestDB(t *testing.T, databaseURL string) *gorm.DB {
	t.Helper()
	admin, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	schema := "auth_withdrawal_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	if err := db.AutoMigrate(&User{}, &EmailAccount{}, &OAuthAccount{}, &RefreshSession{}, &StreamingAccount{}); err != nil {
		t.Fatal(err)
	}
	return db
}
