package auth

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"inno-live-server/internal/plan"

	"github.com/google/uuid"
)

// 프로덕션은 AutoMigrate만 돈다. plan 컬럼이 없던 시절의 사용자가 컬럼 추가
// 뒤 spark로 읽히는지를 실제 PostgreSQL에서 확인한다.
func TestPostgresPlanStoreExistingUserDefaultsToSpark(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL plan store integration test")
	}
	db := newPostgresRefreshTestDB(t, databaseURL)
	if err := db.Exec("ALTER TABLE users DROP CONSTRAINT chk_users_plan, DROP COLUMN plan").Error; err != nil {
		t.Fatal(err)
	}
	legacyID := uuid.New()
	if err := db.Exec("INSERT INTO users (id, status, created_at, updated_at) VALUES (?, 'active', now(), now())", legacyID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&User{}); err != nil {
		t.Fatal(err)
	}

	store := NewPlanStore(db)
	got, err := store.UserPlan(context.Background(), legacyID)
	if err != nil || got != plan.Spark {
		t.Fatalf("legacy user plan = %q, %v; want spark", got, err)
	}
}

func TestPostgresPlanStoreSetAndReject(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL plan store integration test")
	}
	db := newPostgresRefreshTestDB(t, databaseURL)
	now := time.Now().UTC()
	user := User{ID: uuid.New(), Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	store := NewPlanStore(db)
	ctx := context.Background()

	if got, err := store.UserPlan(ctx, user.ID); err != nil || got != plan.Spark {
		t.Fatalf("new user plan = %q, %v; want spark", got, err)
	}
	if err := store.SetUserPlan(ctx, user.ID, plan.Plasma); err != nil {
		t.Fatal(err)
	}
	if got, err := store.UserPlan(ctx, user.ID); err != nil || got != plan.Plasma {
		t.Fatalf("updated plan = %q, %v; want plasma", got, err)
	}
	if err := store.SetUserPlan(ctx, user.ID, "gold"); err == nil {
		t.Fatal("unknown plan must be rejected")
	}
	// CHECK 제약이 코드 검증을 우회한 쓰기도 막는다.
	if err := db.Exec("UPDATE users SET plan = 'gold' WHERE id = ?", user.ID).Error; err == nil {
		t.Fatal("database must reject unknown plan")
	}
	missing := uuid.New()
	if err := store.SetUserPlan(ctx, missing, plan.Beam); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("set on missing user = %v; want ErrUserNotFound", err)
	}
	if _, err := store.UserPlan(ctx, missing); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("read on missing user = %v; want ErrUserNotFound", err)
	}
}
