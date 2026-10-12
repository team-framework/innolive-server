package billing

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/plan"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestApplyGrantsPlanAndIgnoresDuplicateEvent(t *testing.T) {
	db := newPostgresBillingTestDB(t)
	service, now := newTestService(db)
	userID := createTestUser(t, db, plan.Spark)
	event := subscriptionEvent(userID, ChannelPaddle, "evt-1", "sub-1", plan.Beam, StatusActive, now.Add(30*24*time.Hour))

	result, err := service.Apply(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if result.Duplicate || result.Plan != plan.Beam {
		t.Fatalf("first apply = %+v", result)
	}

	// 같은 이벤트 재전송은 내용이 달라도 반영하지 않는다.
	event.Plan = plan.Plasma
	result, err = service.Apply(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Duplicate {
		t.Fatalf("second apply = %+v, want duplicate", result)
	}
	assertUserPlan(t, db, userID, plan.Beam)
	var events int64
	db.Model(&Event{}).Count(&events)
	if events != 1 {
		t.Fatalf("events = %d, want 1", events)
	}
}

func TestApplyUsesHighestEntitledPlanAcrossChannels(t *testing.T) {
	db := newPostgresBillingTestDB(t)
	service, now := newTestService(db)
	userID := createTestUser(t, db, plan.Spark)
	ctx := context.Background()

	mustApply(t, service, subscriptionEvent(userID, ChannelAppStore, "a-1", "ios-sub", plan.Plasma, StatusActive, now.Add(time.Hour)))
	mustApply(t, service, subscriptionEvent(userID, ChannelPaddle, "p-1", "web-sub", plan.Glow, StatusActive, now.Add(time.Hour)))
	assertUserPlan(t, db, userID, plan.Plasma)

	// 해지 예약은 기간이 끝날 때까지 플랜을 유지한다.
	mustApply(t, service, subscriptionEvent(userID, ChannelAppStore, "a-2", "ios-sub", plan.Plasma, StatusCanceled, now.Add(time.Hour)))
	assertUserPlan(t, db, userID, plan.Plasma)

	// 앱 구독이 만료되면 남은 웹 구독 플랜으로 내려간다.
	result, err := service.Apply(ctx, subscriptionEvent(userID, ChannelAppStore, "a-3", "ios-sub", plan.Plasma, StatusExpired, now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Plan != plan.Glow {
		t.Fatalf("plan after app expiry = %s, want glow", result.Plan)
	}
	assertUserPlan(t, db, userID, plan.Glow)
}

func TestExpireDueReturnsUserToSparkAndKeepsAdminPlans(t *testing.T) {
	db := newPostgresBillingTestDB(t)
	service, now := newTestService(db)
	subscriber := createTestUser(t, db, plan.Spark)
	adminGranted := createTestUser(t, db, plan.Beam)

	mustApply(t, service, subscriptionEvent(subscriber, ChannelOneStore, "o-1", "one-sub", plan.Beam, StatusCanceled, now.Add(time.Minute)))
	assertUserPlan(t, db, subscriber, plan.Beam)

	// 기간이 끝나기 전에는 만료되지 않는다.
	if count, err := service.ExpireDue(context.Background()); err != nil || count != 0 {
		t.Fatalf("early expire = %d, %v", count, err)
	}

	service.now = func() time.Time { return now.Add(2 * time.Minute) }
	count, err := service.ExpireDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expired = %d, want 1", count)
	}
	assertUserPlan(t, db, subscriber, plan.Spark)
	// 구독 기록이 없는 사용자는 관리자가 지정한 플랜 그대로다.
	assertUserPlan(t, db, adminGranted, plan.Beam)

	var subscription Subscription
	if err := db.Where("external_id = ?", "one-sub").Take(&subscription).Error; err != nil {
		t.Fatal(err)
	}
	if subscription.Status != StatusExpired {
		t.Fatalf("status = %s, want expired", subscription.Status)
	}
}

func TestApplyRejectsInvalidEventsAndForeignSubscription(t *testing.T) {
	db := newPostgresBillingTestDB(t)
	service, now := newTestService(db)
	owner := createTestUser(t, db, plan.Spark)
	other := createTestUser(t, db, plan.Spark)
	valid := subscriptionEvent(owner, ChannelPaddle, "evt-1", "sub-1", plan.Beam, StatusActive, now.Add(time.Hour))

	invalid := map[string]func(*SubscriptionEvent){
		"channel":    func(e *SubscriptionEvent) { e.Channel = "google_play" },
		"spark plan": func(e *SubscriptionEvent) { e.Plan = plan.Spark },
		"plan":       func(e *SubscriptionEvent) { e.Plan = "gold" },
		"status":     func(e *SubscriptionEvent) { e.Status = "paused" },
		"event id":   func(e *SubscriptionEvent) { e.EventID = " " },
		"sub id":     func(e *SubscriptionEvent) { e.ExternalID = "" },
		"user":       func(e *SubscriptionEvent) { e.UserID = uuid.Nil },
		"period end": func(e *SubscriptionEvent) { e.CurrentPeriodEnd = time.Time{} },
	}
	for name, mutate := range invalid {
		event := valid
		mutate(&event)
		if _, err := service.Apply(context.Background(), event); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("%s: err = %v, want ErrInvalidEvent", name, err)
		}
	}

	missing := valid
	missing.UserID = uuid.New()
	if _, err := service.Apply(context.Background(), missing); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("missing user err = %v", err)
	}

	mustApply(t, service, valid)
	stolen := subscriptionEvent(other, ChannelPaddle, "evt-2", "sub-1", plan.Plasma, StatusActive, now.Add(time.Hour))
	if _, err := service.Apply(context.Background(), stolen); !errors.Is(err, ErrSubscriptionOwnerMismatch) {
		t.Fatalf("foreign subscription err = %v", err)
	}
	assertUserPlan(t, db, other, plan.Spark)
	// 거절된 이벤트는 트랜잭션째 되돌아가 기록도 남지 않는다.
	var events int64
	db.Model(&Event{}).Where("external_event_id = ?", "evt-2").Count(&events)
	if events != 0 {
		t.Fatalf("rejected event recorded %d times", events)
	}
}

func newTestService(db *gorm.DB) (*Service, time.Time) {
	now := time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)
	service := NewService(db)
	service.now = func() time.Time { return now }
	return service, now
}

func subscriptionEvent(userID uuid.UUID, channel Channel, eventID, subscriptionID string, value plan.Plan, status Status, periodEnd time.Time) SubscriptionEvent {
	return SubscriptionEvent{
		Channel:          channel,
		EventID:          eventID,
		Type:             "subscription.updated",
		UserID:           userID,
		ExternalID:       subscriptionID,
		Plan:             value,
		Status:           status,
		CurrentPeriodEnd: periodEnd,
	}
}

func mustApply(t *testing.T, service *Service, event SubscriptionEvent) {
	t.Helper()
	if _, err := service.Apply(context.Background(), event); err != nil {
		t.Fatal(err)
	}
}

func assertUserPlan(t *testing.T, db *gorm.DB, userID uuid.UUID, want plan.Plan) {
	t.Helper()
	var user auth.User
	if err := db.Where("id = ?", userID).Take(&user).Error; err != nil {
		t.Fatal(err)
	}
	if user.Plan != want {
		t.Fatalf("user plan = %s, want %s", user.Plan, want)
	}
}

func createTestUser(t *testing.T, db *gorm.DB, value plan.Plan) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	user := auth.User{ID: uuid.New(), Status: auth.UserStatusActive, Plan: value, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	return user.ID
}

func newPostgresBillingTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL billing integration test")
	}
	admin, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	schema := "billing_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.AutoMigrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := AutoMigrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}
