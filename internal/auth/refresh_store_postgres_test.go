package auth

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// TestPostgresRefreshRotationConcurrentReuse verifies the PostgreSQL-specific
// locking and persistence contract. Set TEST_DATABASE_URL to a database the
// test user may create and drop schemas in (for example the compose Postgres
// instance) to run it. Each run uses an isolated, temporary schema.
func TestPostgresRefreshRotationConcurrentReuse(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL refresh rotation integration test")
	}

	db := newPostgresRefreshTestDB(t, databaseURL)
	now := time.Now().UTC().Truncate(time.Microsecond)
	user := User{ID: uuid.New(), Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	statusChecker := NewGormUserStatusChecker(db)
	if status, err := statusChecker.UserStatus(context.Background(), user.ID); err != nil || status != UserStatusActive {
		t.Fatalf("active user status = %q, %v", status, err)
	}
	if _, err := statusChecker.UserStatus(context.Background(), uuid.New()); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("missing user status error = %v, want ErrUserInactive", err)
	}
	if err := db.Model(&User{}).Where("id = ?", user.ID).Update("status", UserStatusDisabled).Error; err != nil {
		t.Fatal(err)
	}
	if status, err := statusChecker.UserStatus(context.Background(), user.ID); err != nil || status != UserStatusDisabled {
		t.Fatalf("disabled user status = %q, %v", status, err)
	}
	if err := db.Model(&User{}).Where("id = ?", user.ID).Update("status", UserStatusActive).Error; err != nil {
		t.Fatal(err)
	}

	service := NewTokenService(db, testTokenService(newMemoryRefreshStore()).cfg)
	service.now = func() time.Time { return now }
	first, err := service.IssuePair(context.Background(), user.ID, ClientInfo{
		UserAgent: "PostgreSQL integration test",
		IPAddress: "127.0.0.1",
	})
	if err != nil {
		t.Fatal(err)
	}

	type rotationResult struct {
		pair TokenPair
		err  error
	}
	start := make(chan struct{})
	results := make(chan rotationResult, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			pair, rotateErr := service.Rotate(context.Background(), first.RefreshToken, ClientInfo{
				UserAgent: "PostgreSQL integration test",
				IPAddress: "127.0.0.1",
			})
			results <- rotationResult{pair: pair, err: rotateErr}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	var second TokenPair
	successes := 0
	reuses := 0
	for result := range results {
		switch {
		case result.err == nil:
			successes++
			second = result.pair
		case errors.Is(result.err, ErrRefreshTokenReused):
			reuses++
		default:
			t.Fatalf("concurrent rotation error = %v", result.err)
		}
	}
	if successes != 1 || reuses != 1 {
		t.Fatalf("rotation outcomes: successes=%d reuses=%d, want 1 each", successes, reuses)
	}

	if _, err := service.Rotate(context.Background(), second.RefreshToken, ClientInfo{}); !errors.Is(err, ErrRefreshTokenRevoked) {
		t.Fatalf("rotating R2 after R1 reuse = %v, want ErrRefreshTokenRevoked", err)
	}

	firstHash, err := hashRefreshToken(first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := hashRefreshToken(second.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	var original, replacement refreshSessionRow
	if err := db.Where("token_hash = ?", firstHash).Take(&original).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("token_hash = ?", secondHash).Take(&replacement).Error; err != nil {
		t.Fatal(err)
	}
	if len(original.TokenHash) != 32 || len(replacement.TokenHash) != 32 {
		t.Fatalf("BYTEA token hash lengths = %d, %d; want 32", len(original.TokenHash), len(replacement.TokenHash))
	}
	if original.IPAddress == nil || *original.IPAddress != "127.0.0.1" {
		t.Fatalf("INET IP address = %v, want 127.0.0.1", original.IPAddress)
	}
	if original.ReplacedByID == nil || *original.ReplacedByID != replacement.ID {
		t.Fatalf("replaced_by_id = %v, want %s", original.ReplacedByID, replacement.ID)
	}
	if replacement.RevokeReason == nil || *replacement.RevokeReason != revokeReasonReuseDetected {
		t.Fatalf("R2 revoke reason = %v, want %q", replacement.RevokeReason, revokeReasonReuseDetected)
	}
}

func newPostgresRefreshTestDB(t *testing.T, databaseURL string) *gorm.DB {
	t.Helper()
	admin, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	schema := "auth_refresh_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	if err := db.AutoMigrate(&User{}, &RefreshSession{}); err != nil {
		t.Fatal(err)
	}
	return db
}
