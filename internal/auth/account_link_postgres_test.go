package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"inno-live-server/internal/plan"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

func newAccountLinkTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL account link integration test")
	}
	db := newPostgresRefreshTestDB(t, databaseURL)
	if err := db.AutoMigrate(&OAuthAccount{}, &EmailAccount{}, &StreamingAccount{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func createLinkTestUser(t *testing.T, db *gorm.DB, email string, value plan.Plan) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	user := User{ID: uuid.New(), Status: UserStatusActive, Plan: value, CreatedAt: now, UpdatedAt: now}
	if email != "" {
		user.Email = &email
	}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if email != "" {
		if err := db.Create(&EmailAccount{UserID: user.ID, Email: email, PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return user.ID
}

func createLinkTestIdentity(t *testing.T, db *gorm.DB, userID uuid.UUID, provider OAuthProvider, subject string) {
	t.Helper()
	account := newLinkedOAuthAccount(userID, LinkIdentity{Provider: provider, Subject: subject}, time.Now().UTC())
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
}

func createLinkTestStreaming(t *testing.T, db *gorm.DB, userID uuid.UUID, provider StreamingProvider, channel string) {
	t.Helper()
	now := time.Now().UTC()
	account := StreamingAccount{ID: uuid.New(), UserID: userID, Provider: provider, ChannelID: channel, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
}

func TestPostgresLinkAddsIdentityAndRejectsConflicts(t *testing.T) {
	db := newAccountLinkTestDB(t)
	store := NewAccountLinkStore(db)
	ctx := context.Background()
	member := createLinkTestUser(t, db, "member@example.com", plan.Spark)
	other := createLinkTestUser(t, db, "other@example.com", plan.Spark)
	createLinkTestIdentity(t, db, other, OAuthProviderGoogle, "other-google")

	if _, err := store.Link(ctx, member, LinkIdentity{Provider: OAuthProviderGoogle, Subject: "member-google", Email: "member@gmail.com"}); err != nil {
		t.Fatalf("link new identity: %v", err)
	}
	// 같은 신원을 다시 연결하면 갱신만 한다.
	if result, err := store.Link(ctx, member, LinkIdentity{Provider: OAuthProviderGoogle, Subject: "member-google"}); err != nil || result.MergedUserID != nil {
		t.Fatalf("relink own identity = %+v, %v", result, err)
	}
	if _, err := store.Link(ctx, member, LinkIdentity{Provider: OAuthProviderGoogle, Subject: "second-google"}); !errors.Is(err, ErrProviderAlreadyLinked) {
		t.Fatalf("second google identity error = %v", err)
	}
	// 이메일 계정을 가진 다른 사용자의 신원은 가져오지 못한다.
	fresh := createLinkTestUser(t, db, "fresh@example.com", plan.Spark)
	if _, err := store.Link(ctx, fresh, LinkIdentity{Provider: OAuthProviderGoogle, Subject: "other-google"}); !errors.Is(err, ErrIdentityLinkedElsewhere) {
		t.Fatalf("identity owned by email user error = %v, want ErrIdentityLinkedElsewhere", err)
	}
	var owner OAuthAccount
	if err := db.Where("provider_subject = ?", "other-google").Take(&owner).Error; err != nil || owner.UserID != other {
		t.Fatalf("rejected link moved the identity: %+v %v", owner, err)
	}
}

func TestPostgresLinkMergesOAuthOnlyUser(t *testing.T) {
	db := newAccountLinkTestDB(t)
	if err := db.Exec("CREATE TABLE usage_sessions (id uuid PRIMARY KEY, user_id uuid)").Error; err != nil {
		t.Fatal(err)
	}
	store := NewAccountLinkStore(db)
	ctx := context.Background()
	member := createLinkTestUser(t, db, "member@example.com", plan.Glow)
	legacy := createLinkTestUser(t, db, "", plan.Plasma)
	createLinkTestIdentity(t, db, legacy, OAuthProviderGoogle, "legacy-google")
	createLinkTestStreaming(t, db, member, StreamingProviderYouTube, "member-channel")
	createLinkTestStreaming(t, db, legacy, StreamingProviderYouTube, "legacy-channel")
	createLinkTestStreaming(t, db, legacy, StreamingProviderChzzk, "legacy-chzzk")
	usageID := uuid.New()
	if err := db.Exec("INSERT INTO usage_sessions (id, user_id) VALUES (?, ?)", usageID, legacy).Error; err != nil {
		t.Fatal(err)
	}

	result, err := store.Link(ctx, member, LinkIdentity{Provider: OAuthProviderGoogle, Subject: "legacy-google"})
	if err != nil {
		t.Fatalf("merge link: %v", err)
	}
	if result.MergedUserID == nil || *result.MergedUserID != legacy {
		t.Fatalf("merged user = %v, want %v", result.MergedUserID, legacy)
	}

	var count int64
	db.Model(&User{}).Where("id = ?", legacy).Count(&count)
	if count != 0 {
		t.Fatal("merged user still exists")
	}
	var identity OAuthAccount
	if err := db.Where("provider_subject = ?", "legacy-google").Take(&identity).Error; err != nil || identity.UserID != member {
		t.Fatalf("identity owner = %+v, %v", identity, err)
	}
	var streams []StreamingAccount
	db.Where("user_id = ?", member).Order("provider").Find(&streams)
	if len(streams) != 2 {
		t.Fatalf("streaming accounts = %+v, want member youtube + moved chzzk", streams)
	}
	for _, stream := range streams {
		if stream.Provider == StreamingProviderYouTube && stream.ChannelID != "member-channel" {
			t.Fatalf("overlapping youtube replaced member's own: %+v", stream)
		}
		if stream.Provider == StreamingProviderChzzk && stream.ChannelID != "legacy-chzzk" {
			t.Fatalf("chzzk not moved: %+v", stream)
		}
	}
	var usageOwner string
	if err := db.Raw("SELECT user_id::text FROM usage_sessions WHERE id = ?", usageID).Scan(&usageOwner).Error; err != nil || usageOwner != member.String() {
		t.Fatalf("usage owner = %v, %v", usageOwner, err)
	}
	var merged User
	db.Where("id = ?", member).Take(&merged)
	if merged.Plan != plan.Plasma {
		t.Fatalf("plan after merge = %q, want plasma", merged.Plan)
	}
}

func TestPostgresMergeRejectsWhenProvidersCollide(t *testing.T) {
	db := newAccountLinkTestDB(t)
	store := NewAccountLinkStore(db)
	ctx := context.Background()
	member := createLinkTestUser(t, db, "member@example.com", plan.Spark)
	createLinkTestIdentity(t, db, member, OAuthProviderApple, "member-apple")
	legacy := createLinkTestUser(t, db, "", plan.Spark)
	createLinkTestIdentity(t, db, legacy, OAuthProviderGoogle, "legacy-google")
	createLinkTestIdentity(t, db, legacy, OAuthProviderApple, "legacy-apple")

	if _, err := store.Link(ctx, member, LinkIdentity{Provider: OAuthProviderGoogle, Subject: "legacy-google"}); !errors.Is(err, ErrProviderAlreadyLinked) {
		t.Fatalf("colliding merge error = %v", err)
	}
	var count int64
	db.Model(&User{}).Where("id = ?", legacy).Count(&count)
	if count != 1 {
		t.Fatal("rejected merge deleted the legacy user")
	}
}

func TestPostgresUnlinkKeepsLastLoginMethod(t *testing.T) {
	db := newAccountLinkTestDB(t)
	store := NewAccountLinkStore(db)
	ctx := context.Background()
	member := createLinkTestUser(t, db, "member@example.com", plan.Spark)
	createLinkTestIdentity(t, db, member, OAuthProviderGoogle, "member-google")
	legacy := createLinkTestUser(t, db, "", plan.Spark)
	createLinkTestIdentity(t, db, legacy, OAuthProviderGoogle, "legacy-google")

	if err := store.Unlink(ctx, legacy, OAuthProviderGoogle); !errors.Is(err, ErrLastLoginMethod) {
		t.Fatalf("unlink last method error = %v", err)
	}
	if err := store.Unlink(ctx, member, OAuthProviderApple); !errors.Is(err, ErrIdentityNotLinked) {
		t.Fatalf("unlink missing provider error = %v", err)
	}
	if err := store.Unlink(ctx, member, OAuthProviderGoogle); err != nil {
		t.Fatalf("unlink google: %v", err)
	}
	methods, err := store.Methods(ctx, member)
	if err != nil || methods.Email == nil || *methods.Email != "member@example.com" || len(methods.Providers) != 0 {
		t.Fatalf("methods after unlink = %+v, %v", methods, err)
	}
}

func TestPostgresResolversDoNotCreateUsers(t *testing.T) {
	db := newAccountLinkTestDB(t)
	ctx := context.Background()
	google := NewGormGoogleAccountResolver(db)
	if _, err := google.ResolveGoogleIdentity(ctx, GoogleIdentity{Subject: "unknown"}); !errors.Is(err, ErrAccountNotLinked) {
		t.Fatalf("unknown google subject error = %v", err)
	}
	apple := NewGormAppleAccountResolver(db)
	if _, err := apple.ResolveAppleIdentity(ctx, AppleIdentity{Subject: "unknown"}, nil, nil); !errors.Is(err, ErrAccountNotLinked) {
		t.Fatalf("unknown apple subject error = %v", err)
	}
	var count int64
	db.Model(&User{}).Count(&count)
	if count != 0 {
		t.Fatalf("resolver created %d users", count)
	}

	member := createLinkTestUser(t, db, "member@example.com", plan.Spark)
	createLinkTestIdentity(t, db, member, OAuthProviderGoogle, "member-google")
	legacy := createLinkTestUser(t, db, "", plan.Spark)
	createLinkTestIdentity(t, db, legacy, OAuthProviderGoogle, "legacy-google")
	if user, err := google.ResolveGoogleIdentity(ctx, GoogleIdentity{Subject: "member-google", Email: "member@gmail.com", EmailVerified: true}); err != nil || user.ID != member || !user.HasEmailAccount {
		t.Fatalf("linked member = %+v, %v", user, err)
	}
	var stored User
	db.Where("id = ?", member).Take(&stored)
	if stored.Email == nil || *stored.Email != "member@example.com" {
		t.Fatalf("google login overwrote the account email: %v", stored.Email)
	}
	if user, err := google.ResolveGoogleIdentity(ctx, GoogleIdentity{Subject: "legacy-google"}); err != nil || user.ID != legacy || user.HasEmailAccount {
		t.Fatalf("legacy user = %+v, %v", user, err)
	}
}

func TestPostgresAttachEmailAccount(t *testing.T) {
	db := newAccountLinkTestDB(t)
	ctx := context.Background()
	accounts := NewGormEmailAccountStore(db)
	createLinkTestUser(t, db, "taken@example.com", plan.Spark)
	legacy := createLinkTestUser(t, db, "", plan.Beam)
	now := time.Now().UTC()

	if err := accounts.AttachEmailAccount(ctx, legacy, PendingEmailSignup{Email: "taken@example.com", PasswordHash: "hash"}, now); !errors.Is(err, ErrEmailAlreadyRegistered) {
		t.Fatalf("attach taken email error = %v", err)
	}
	if err := accounts.AttachEmailAccount(ctx, legacy, PendingEmailSignup{Email: "legacy@example.com", PasswordHash: "hash"}, now); err != nil {
		t.Fatalf("attach email: %v", err)
	}
	account, user, err := accounts.FindEmailAccount(ctx, "legacy@example.com")
	if err != nil || account.UserID != legacy || user.Plan != plan.Beam {
		t.Fatalf("attached account = %+v %+v %v", account, user, err)
	}
	if err := accounts.AttachEmailAccount(ctx, legacy, PendingEmailSignup{Email: "again@example.com", PasswordHash: "hash"}, now); !errors.Is(err, ErrEmailAlreadyRegistered) {
		t.Fatalf("second attach error = %v", err)
	}
}

func serveLinkRequest(t *testing.T, handler http.Handler, method, path, accessToken string, value any) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader = http.NoBody
	if value != nil {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("Content-Type", "application/json")
	if accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestPostgresLinkHTTPMergesAndClosesMergedSessions(t *testing.T) {
	db := newAccountLinkTestDB(t)
	member := createLinkTestUser(t, db, "member@example.com", plan.Spark)
	legacy := createLinkTestUser(t, db, "", plan.Spark)
	createLinkTestIdentity(t, db, legacy, OAuthProviderGoogle, "legacy-google")

	tokens := testTokenService(newMemoryRefreshStore())
	links := NewAccountLinkStore(db)
	google, err := NewGoogleLoginService(&stubGoogleVerifier{identity: GoogleIdentity{Subject: "legacy-google", Email: "legacy@gmail.com", EmailVerified: true}}, NewGormGoogleAccountResolver(db), tokens)
	if err != nil {
		t.Fatal(err)
	}
	google.SetAccountLinks(links)
	var closed []uuid.UUID
	config, _ := NewTokenHTTPConfig(false, nil)
	handler := MountAccountLinkHTTP(http.NotFoundHandler(), tokens, links, google, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), config, func(id uuid.UUID) { closed = append(closed, id) })
	pair, err := tokens.IssuePair(context.Background(), member, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}

	for _, request := range []struct{ method, path string }{
		{http.MethodPost, "/auth/link/google"},
		{http.MethodDelete, "/auth/link/google"},
		{http.MethodGet, "/auth/login-methods"},
	} {
		if response := serveLinkRequest(t, handler, request.method, request.path, "", map[string]string{"id_token": "x"}); response.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s = %d", request.method, request.path, response.Code)
		}
	}
	if response := serveLinkRequest(t, handler, http.MethodDelete, "/auth/link/facebook", pair.AccessToken, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider unlink = %d", response.Code)
	}

	response := serveLinkRequest(t, handler, http.MethodPost, "/auth/link/google", pair.AccessToken, map[string]string{"id_token": "google-id-token"})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"merged":true`) {
		t.Fatalf("link google = %d %s", response.Code, response.Body.String())
	}
	if len(closed) != 1 || closed[0] != legacy {
		t.Fatalf("closed sessions for %v, want merged user %v", closed, legacy)
	}
	methods := serveLinkRequest(t, handler, http.MethodGet, "/auth/login-methods", pair.AccessToken, nil)
	if methods.Code != http.StatusOK || !strings.Contains(methods.Body.String(), `"provider":"google"`) || !strings.Contains(methods.Body.String(), "member@example.com") {
		t.Fatalf("login methods = %d %s", methods.Code, methods.Body.String())
	}
	// 연결한 뒤에는 구글 로그인이 이메일 계정 사용자로 들어간다.
	if _, err := google.Login(context.Background(), "google-id-token", ClientInfo{}); err != nil {
		t.Fatalf("google login after link: %v", err)
	}
}
