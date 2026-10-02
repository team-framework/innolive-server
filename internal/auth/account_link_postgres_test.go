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
	// 구글은 여러 개 연결할 수 있다(#392). 상한을 넘는 연결은 거절한다.
	for i := 2; i <= maxGoogleLogins; i++ {
		if _, err := store.Link(ctx, member, LinkIdentity{Provider: OAuthProviderGoogle, Subject: "google-" + string(rune('0'+i))}); err != nil {
			t.Fatalf("link google #%d: %v", i, err)
		}
	}
	if _, err := store.Link(ctx, member, LinkIdentity{Provider: OAuthProviderGoogle, Subject: "one-too-many"}); !errors.Is(err, ErrGoogleLinkLimit) {
		t.Fatalf("google over limit error = %v, want ErrGoogleLinkLimit", err)
	}
	// 애플은 계정당 1개다.
	if _, err := store.Link(ctx, member, LinkIdentity{Provider: OAuthProviderApple, Subject: "member-apple"}); err != nil {
		t.Fatalf("link apple: %v", err)
	}
	if _, err := store.Link(ctx, member, LinkIdentity{Provider: OAuthProviderApple, Subject: "second-apple"}); !errors.Is(err, ErrProviderAlreadyLinked) {
		t.Fatalf("second apple identity error = %v", err)
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

func TestPostgresMergeMovesGoogleUnlessOverLimit(t *testing.T) {
	db := newAccountLinkTestDB(t)
	store := NewAccountLinkStore(db)
	ctx := context.Background()
	member := createLinkTestUser(t, db, "member@example.com", plan.Spark)
	createLinkTestIdentity(t, db, member, OAuthProviderGoogle, "member-google")
	legacy := createLinkTestUser(t, db, "", plan.Spark)
	createLinkTestIdentity(t, db, legacy, OAuthProviderGoogle, "legacy-google")

	// 구글끼리는 겹쳐도 병합한다.
	if result, err := store.Link(ctx, member, LinkIdentity{Provider: OAuthProviderGoogle, Subject: "legacy-google"}); err != nil || result.MergedUserID == nil {
		t.Fatalf("google merge = %+v, %v", result, err)
	}
	var googles int64
	db.Model(&OAuthAccount{}).Where("user_id = ? AND provider = ?", member, OAuthProviderGoogle).Count(&googles)
	if googles != 2 {
		t.Fatalf("google links after merge = %d, want 2", googles)
	}

	// 병합 후 상한을 넘으면 병합 전체를 거절한다.
	for i := 3; i <= maxGoogleLogins; i++ {
		createLinkTestIdentity(t, db, member, OAuthProviderGoogle, "member-google-"+string(rune('0'+i)))
	}
	full := createLinkTestUser(t, db, "", plan.Spark)
	createLinkTestIdentity(t, db, full, OAuthProviderGoogle, "full-google")
	if _, err := store.Link(ctx, member, LinkIdentity{Provider: OAuthProviderGoogle, Subject: "full-google"}); !errors.Is(err, ErrGoogleLinkLimit) {
		t.Fatalf("merge over limit error = %v, want ErrGoogleLinkLimit", err)
	}
	var remaining int64
	db.Model(&User{}).Where("id = ?", full).Count(&remaining)
	if remaining != 1 {
		t.Fatal("rejected merge deleted the legacy user")
	}
}

func TestPostgresUnlinkByIDWhenSeveralGoogleLinks(t *testing.T) {
	db := newAccountLinkTestDB(t)
	store := NewAccountLinkStore(db)
	ctx := context.Background()
	member := createLinkTestUser(t, db, "member@example.com", plan.Spark)
	createLinkTestIdentity(t, db, member, OAuthProviderGoogle, "first-google")
	createLinkTestIdentity(t, db, member, OAuthProviderGoogle, "second-google")
	other := createLinkTestUser(t, db, "other@example.com", plan.Spark)
	createLinkTestIdentity(t, db, other, OAuthProviderGoogle, "other-google")

	if err := store.Unlink(ctx, member, OAuthProviderGoogle, nil); !errors.Is(err, ErrMultipleLinks) {
		t.Fatalf("unlink without id error = %v, want ErrMultipleLinks", err)
	}
	var foreign OAuthAccount
	db.Where("provider_subject = ?", "other-google").Take(&foreign)
	if err := store.Unlink(ctx, member, OAuthProviderGoogle, &foreign.ID); !errors.Is(err, ErrIdentityNotLinked) {
		t.Fatalf("unlink other user's link error = %v, want ErrIdentityNotLinked", err)
	}
	methods, err := store.Methods(ctx, member)
	if err != nil || len(methods.Providers) != 2 || methods.Providers[0].ID == uuid.Nil {
		t.Fatalf("methods = %+v, %v", methods, err)
	}
	if err := store.Unlink(ctx, member, OAuthProviderGoogle, &methods.Providers[0].ID); err != nil {
		t.Fatalf("unlink by id: %v", err)
	}
	// 하나 남으면 id 없이도 끊을 수 있다.
	if err := store.Unlink(ctx, member, OAuthProviderGoogle, nil); err != nil {
		t.Fatalf("unlink last google without id: %v", err)
	}
}

// 기동 시 AutoMigrate가 옛 사용자·공급자 고유 키를 제약·인덱스 어느 형태든 지우고
// 애플 전용 부분 고유 인덱스로 바꾼다(#392).
func TestPostgresAutoMigrateSwapsOAuthUserProviderUnique(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL migration integration test")
	}
	for _, legacy := range []string{
		"ALTER TABLE oauth_accounts ADD CONSTRAINT uidx_oauth_user_provider UNIQUE (user_id, provider)",
		"CREATE UNIQUE INDEX uidx_oauth_user_provider ON oauth_accounts (user_id, provider)",
	} {
		db := newPostgresRefreshTestDB(t, databaseURL)
		ctx := context.Background()
		if err := AutoMigrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("DROP INDEX uidx_oauth_user_apple").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(legacy).Error; err != nil {
			t.Fatal(err)
		}
		if err := AutoMigrate(ctx, db); err != nil {
			t.Fatalf("auto migrate over %q: %v", legacy, err)
		}
		var names []string
		db.Raw("SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'oauth_accounts'").Scan(&names)
		joined := strings.Join(names, ",")
		if strings.Contains(joined, "uidx_oauth_user_provider") || !strings.Contains(joined, "uidx_oauth_user_apple") {
			t.Fatalf("indexes after %q = %s", legacy, joined)
		}

		user := createLinkTestUser(t, db, "", plan.Spark)
		createLinkTestIdentity(t, db, user, OAuthProviderGoogle, "google-a")
		createLinkTestIdentity(t, db, user, OAuthProviderGoogle, "google-b")
		createLinkTestIdentity(t, db, user, OAuthProviderApple, "apple-a")
		second := newLinkedOAuthAccount(user, LinkIdentity{Provider: OAuthProviderApple, Subject: "apple-b"}, time.Now().UTC())
		if err := db.Create(&second).Error; err == nil {
			t.Fatalf("database accepted a second apple link after %q", legacy)
		}
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

	if err := store.Unlink(ctx, legacy, OAuthProviderGoogle, nil); !errors.Is(err, ErrLastLoginMethod) {
		t.Fatalf("unlink last method error = %v", err)
	}
	if err := store.Unlink(ctx, member, OAuthProviderApple, nil); !errors.Is(err, ErrIdentityNotLinked) {
		t.Fatalf("unlink missing provider error = %v", err)
	}
	if err := store.Unlink(ctx, member, OAuthProviderGoogle, nil); err != nil {
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
	if _, err := google.ResolveGoogleIdentity(ctx, GoogleIdentity{Subject: "unknown"}, false); !errors.Is(err, ErrAccountNotLinked) {
		t.Fatalf("unknown google subject error = %v", err)
	}
	apple := NewGormAppleAccountResolver(db)
	if _, err := apple.ResolveAppleIdentity(ctx, AppleIdentity{Subject: "unknown"}, nil, nil, false); !errors.Is(err, ErrAccountNotLinked) {
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
	if user, err := google.ResolveGoogleIdentity(ctx, GoogleIdentity{Subject: "member-google", Email: "member@gmail.com", EmailVerified: true}, false); err != nil || user.ID != member || !user.HasEmailAccount {
		t.Fatalf("linked member = %+v, %v", user, err)
	}
	var stored User
	db.Where("id = ?", member).Take(&stored)
	if stored.Email == nil || *stored.Email != "member@example.com" {
		t.Fatalf("google login overwrote the account email: %v", stored.Email)
	}
	if user, err := google.ResolveGoogleIdentity(ctx, GoogleIdentity{Subject: "legacy-google"}, false); err != nil || user.ID != legacy || user.HasEmailAccount {
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
	var linked struct {
		Providers []struct {
			ID string `json:"id"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(methods.Body.Bytes(), &linked); err != nil || len(linked.Providers) != 1 || linked.Providers[0].ID == "" {
		t.Fatalf("login methods id = %+v, %v", linked, err)
	}
	createLinkTestIdentity(t, db, member, OAuthProviderGoogle, "member-second-google")
	if response := serveLinkRequest(t, handler, http.MethodDelete, "/auth/link/google", pair.AccessToken, nil); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"multiple_links"`) {
		t.Fatalf("unlink without id = %d %s", response.Code, response.Body.String())
	}
	if response := serveLinkRequest(t, handler, http.MethodDelete, "/auth/link/google/not-a-uuid", pair.AccessToken, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("unlink bad id = %d", response.Code)
	}
	if response := serveLinkRequest(t, handler, http.MethodDelete, "/auth/link/google/"+uuid.NewString(), pair.AccessToken, nil); response.Code != http.StatusNotFound {
		t.Fatalf("unlink unknown id = %d", response.Code)
	}
	var second OAuthAccount
	db.Where("provider_subject = ?", "member-second-google").Take(&second)
	if response := serveLinkRequest(t, handler, http.MethodDelete, "/auth/link/google/"+second.ID.String(), pair.AccessToken, nil); response.Code != http.StatusNoContent {
		t.Fatalf("unlink by id = %d %s", response.Code, response.Body.String())
	}
	// 연결한 뒤에는 v2 구글 로그인이 이메일 계정 사용자로 들어간다.
	if _, err := google.LoginV2(context.Background(), "google-id-token", ClientInfo{}); err != nil {
		t.Fatalf("google login after link: %v", err)
	}
}

func TestPostgresV1ResolverStillCreatesUsers(t *testing.T) {
	db := newAccountLinkTestDB(t)
	ctx := context.Background()
	google := NewGormGoogleAccountResolver(db)
	created, err := google.ResolveGoogleIdentity(ctx, GoogleIdentity{Subject: "v1-google", Email: "v1@gmail.com", EmailVerified: true}, true)
	if err != nil || created.HasEmailAccount {
		t.Fatalf("v1 google create = %+v, %v", created, err)
	}
	var user User
	if err := db.Where("id = ?", created.ID).Take(&user).Error; err != nil || user.Email == nil || *user.Email != "v1@gmail.com" {
		t.Fatalf("v1 user email = %+v, %v", user.Email, err)
	}
	again, err := google.ResolveGoogleIdentity(ctx, GoogleIdentity{Subject: "v1-google"}, true)
	if err != nil || again.ID != created.ID {
		t.Fatalf("v1 second login = %+v, %v", again, err)
	}
	apple := NewGormAppleAccountResolver(db)
	if user, err := apple.ResolveAppleIdentity(ctx, AppleIdentity{Subject: "v1-apple"}, nil, nil, true); err != nil || user.ID == uuid.Nil {
		t.Fatalf("v1 apple create = %+v, %v", user, err)
	}
}

// 구글 로그인은 가입 때 정한 이름을 덮지 않고, 비어 있을 때만 구글 이름으로 채운다(#386).
func TestPostgresGoogleLoginKeepsExistingName(t *testing.T) {
	db := newAccountLinkTestDB(t)
	ctx := context.Background()
	named := createLinkTestUser(t, db, "named@example.com", plan.Spark)
	if err := db.Model(&User{}).Where("id = ?", named).Update("display_name", "홍길동").Error; err != nil {
		t.Fatal(err)
	}
	createLinkTestIdentity(t, db, named, OAuthProviderGoogle, "named-google")
	unnamed := createLinkTestUser(t, db, "unnamed@example.com", plan.Spark)
	createLinkTestIdentity(t, db, unnamed, OAuthProviderGoogle, "unnamed-google")
	google := NewGormGoogleAccountResolver(db)

	for _, subject := range []string{"named-google", "unnamed-google"} {
		if _, err := google.ResolveGoogleIdentity(ctx, GoogleIdentity{Subject: subject, DisplayName: "Google Name"}, false); err != nil {
			t.Fatal(err)
		}
	}
	var users []User
	db.Where("id IN ?", []uuid.UUID{named, unnamed}).Find(&users)
	for _, user := range users {
		want := "Google Name"
		if user.ID == named {
			want = "홍길동"
		}
		if user.DisplayName == nil || *user.DisplayName != want {
			t.Fatalf("user %v name = %v, want %q", user.ID, user.DisplayName, want)
		}
	}
}

func TestPostgresEmailSignupStoresName(t *testing.T) {
	db := newAccountLinkTestDB(t)
	accounts := NewGormEmailAccountStore(db)
	id, err := accounts.CreateEmailUser(context.Background(), PendingEmailSignup{Email: "new@example.com", PasswordHash: "hash", Name: "홍길동"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var user User
	if err := db.Where("id = ?", id).Take(&user).Error; err != nil || user.DisplayName == nil || *user.DisplayName != "홍길동" {
		t.Fatalf("stored name = %v, %v", user.DisplayName, err)
	}
}

// 비밀번호를 바꾸면 그 사용자의 모든 refresh 세션이 폐기된다(#388).
func TestPostgresResetPasswordRevokesAllRefreshSessions(t *testing.T) {
	db := newAccountLinkTestDB(t)
	ctx := context.Background()
	member := createLinkTestUser(t, db, "member@example.com", plan.Spark)
	other := createLinkTestUser(t, db, "other@example.com", plan.Spark)
	tokens := newTokenService(&gormRefreshStore{db: db}, testTokenService(newMemoryRefreshStore()).cfg)
	phone, err := tokens.IssuePair(ctx, member, ClientInfo{UserAgent: "phone"})
	if err != nil {
		t.Fatal(err)
	}
	laptop, err := tokens.IssuePair(ctx, member, ClientInfo{UserAgent: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	untouched, err := tokens.IssuePair(ctx, other, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}

	accounts := NewGormEmailAccountStore(db)
	if err := accounts.ResetPassword(ctx, member, "member@example.com", "new-hash", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string]TokenPair{"phone": phone, "laptop": laptop} {
		if _, err := tokens.Rotate(ctx, pair.RefreshToken, ClientInfo{}); err == nil {
			t.Fatalf("%s refresh token still works after password reset", name)
		}
	}
	if _, err := tokens.Rotate(ctx, untouched.RefreshToken, ClientInfo{}); err != nil {
		t.Fatalf("another user's refresh token was revoked: %v", err)
	}
	var account EmailAccount
	if err := db.Where("user_id = ?", member).Take(&account).Error; err != nil || account.PasswordHash != "new-hash" {
		t.Fatalf("password hash = %q, %v", account.PasswordHash, err)
	}
	if err := accounts.ResetPassword(ctx, member, "changed@example.com", "x", time.Now().UTC()); err == nil {
		t.Fatal("reset with a stale email must fail")
	}
}
