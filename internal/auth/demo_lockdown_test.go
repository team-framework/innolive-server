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
	"testing"
	"time"

	"github.com/google/uuid"
)

func assertDemoLockdownResponse(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", response.Code, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != demoLockdownCode {
		t.Fatalf("error code = %q (%v), want %q", body.Error.Code, err, demoLockdownCode)
	}
}

func TestDemoLockdownAdmit(t *testing.T) {
	admin := uuid.New()
	var off *DemoLockdown
	if off.Enabled() || off.Admit(uuid.New()) != nil {
		t.Fatal("a nil lockdown must be disabled and admit everyone")
	}
	lockdown := NewDemoLockdown([]uuid.UUID{admin})
	if !lockdown.Enabled() {
		t.Fatal("lockdown must be enabled")
	}
	if err := lockdown.Admit(admin); err != nil {
		t.Fatalf("admin rejected: %v", err)
	}
	for _, id := range []uuid.UUID{uuid.New(), uuid.Nil} {
		if err := lockdown.Admit(id); !errors.Is(err, ErrDemoLockdown) {
			t.Fatalf("Admit(%s) = %v, want ErrDemoLockdown", id, err)
		}
	}
}

// 시연 모드는 새 토큰과 refresh 회전을 관리자에게만 허용한다. 거절된 refresh token은
// 폐기하지 않아, 시연이 끝나면 사용자가 다시 로그인하지 않고 그대로 쓴다.
func TestDemoLockdownTokenIssueAndRefresh(t *testing.T) {
	ctx := context.Background()
	service := testTokenService(newMemoryRefreshStore())
	admin, member := uuid.New(), uuid.New()
	adminPair, err := service.IssuePair(ctx, admin, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	memberPair, err := service.IssuePair(ctx, member, ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}

	service.SetDemoLockdown(NewDemoLockdown([]uuid.UUID{admin}))
	if _, err := service.IssuePair(ctx, member, ClientInfo{}); !errors.Is(err, ErrDemoLockdown) {
		t.Fatalf("member IssuePair = %v, want ErrDemoLockdown", err)
	}
	if _, err := service.IssuePair(ctx, admin, ClientInfo{}); err != nil {
		t.Fatalf("admin IssuePair: %v", err)
	}
	if _, err := service.Rotate(ctx, memberPair.RefreshToken, ClientInfo{}); !errors.Is(err, ErrDemoLockdown) {
		t.Fatalf("member Rotate = %v, want ErrDemoLockdown", err)
	}
	if _, err := service.Rotate(ctx, adminPair.RefreshToken, ClientInfo{}); err != nil {
		t.Fatalf("admin Rotate: %v", err)
	}

	service.SetDemoLockdown(nil)
	if _, err := service.Rotate(ctx, memberPair.RefreshToken, ClientInfo{}); err != nil {
		t.Fatalf("member refresh token unusable after lockdown ended: %v", err)
	}
}

func TestDemoLockdownRefreshHTTP(t *testing.T) {
	config, err := NewTokenHTTPConfig(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	service, handler := testTokenHTTPHandler(t, config)
	pair, err := service.IssuePair(context.Background(), uuid.New(), ClientInfo{})
	if err != nil {
		t.Fatal(err)
	}
	service.SetDemoLockdown(NewDemoLockdown([]uuid.UUID{uuid.New()}))

	body, _ := json.Marshal(map[string]string{"refresh_token": pair.RefreshToken})
	request := httptest.NewRequest(http.MethodPost, "/auth/refresh", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertDemoLockdownResponse(t, response)
}

// 로그인만 막으면 이미 토큰을 가진 앱이 그대로 세션을 만든다. 인증 미들웨어도 막는다.
func TestDemoLockdownRequireUser(t *testing.T) {
	service := testTokenService(newMemoryRefreshStore())
	admin, member := uuid.New(), uuid.New()
	adminToken, err := service.issueAccessToken(admin, uuid.New(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	memberToken, err := service.issueAccessToken(member, uuid.New(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	service.SetDemoLockdown(NewDemoLockdown([]uuid.UUID{admin}))
	handler := RequireUser(service, testUserStatusChecker{status: UserStatusActive})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	serve := func(token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/sessions", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	assertDemoLockdownResponse(t, serve(memberToken))
	if response := serve(adminToken); response.Code != http.StatusNoContent {
		t.Fatalf("admin status = %d, want 204", response.Code)
	}
}

func signUpEmailAccount(t *testing.T, handler http.Handler, sender *recordingVerificationEmailSender, email, password string) {
	t.Helper()
	start := serveEmailJSON(t, handler, http.MethodPost, "/auth/v2/native/sign-up", map[string]string{"email": email, "password": password, "name": "회원"}, nil)
	var started struct {
		SignupToken string `json:"signup_token"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil || started.SignupToken == "" {
		t.Fatalf("signup start = %d %s", start.Code, start.Body.String())
	}
	if verify := serveEmailJSON(t, handler, http.MethodPost, "/auth/native/verify-email", map[string]string{"signup_token": started.SignupToken, "verification_code": sender.code}, nil); verify.Code != http.StatusOK {
		t.Fatalf("signup verify = %d %s", verify.Code, verify.Body.String())
	}
}

// 시연 모드는 가입·계정 설정·비밀번호 재설정을 닫고 이메일 로그인을 관리자에게만
// 연다. 끄면 같은 경로가 종전대로 동작한다.
func TestDemoLockdownEmailRoutes(t *testing.T) {
	ctx := context.Background()
	pending := newMemoryPendingEmailSignupStore()
	accounts := &memoryEmailAccountStore{}
	sender := &recordingVerificationEmailSender{}
	tokens := testTokenService(newMemoryRefreshStore())
	service := newTestEmailAuthService(t, pending, accounts, sender, tokens)
	config, _ := NewTokenHTTPConfig(false, nil)
	handler := MountAuthHTTPWithServices(http.NotFoundHandler(), tokens, nil, nil, service, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), config)

	// 메모리 저장소는 계정을 하나만 담는다. 같은 계정을 관리자 목록에서 빼고 넣어
	// 일반 회원과 관리자를 차례로 본다.
	const email, password = "member@example.com", "correct horse battery staple"
	signUpEmailAccount(t, handler, sender, email, password)
	_, user, err := accounts.FindEmailAccount(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	tokens.SetDemoLockdown(NewDemoLockdown([]uuid.UUID{uuid.New()}))
	*sender = recordingVerificationEmailSender{}

	for _, path := range []string{
		"/auth/sign-up",
		"/auth/verify-email",
		"/auth/native/sign-up",
		"/auth/v2/native/sign-up",
		"/auth/native/verify-email",
		"/auth/native/account-setup",
		"/auth/password/reset",
		"/auth/password/reset/verify",
	} {
		t.Run(path, func(t *testing.T) {
			assertDemoLockdownResponse(t, serveEmailJSON(t, handler, http.MethodPost, path, map[string]string{"email": "new@example.com", "password": password, "name": "새 회원"}, nil))
			preflight := httptest.NewRecorder()
			handler.ServeHTTP(preflight, httptest.NewRequest(http.MethodOptions, path, nil))
			if preflight.Code == http.StatusServiceUnavailable {
				t.Fatal("CORS preflight must not be rejected")
			}
		})
	}
	if sender.code != "" {
		t.Fatal("a closed route sent a verification email")
	}

	signIn := map[string]string{"email": email, "password": password}
	assertDemoLockdownResponse(t, serveEmailJSON(t, handler, http.MethodPost, "/auth/sign-in", signIn, nil))
	tokens.SetDemoLockdown(NewDemoLockdown([]uuid.UUID{user.ID}))
	if response := serveEmailJSON(t, handler, http.MethodPost, "/auth/sign-in", signIn, nil); response.Code != http.StatusOK {
		t.Fatalf("admin sign-in = %d %s", response.Code, response.Body.String())
	}

	tokens.SetDemoLockdown(nil)
	if response := serveEmailJSON(t, handler, http.MethodPost, "/auth/v2/native/sign-up", map[string]string{"email": "new@example.com", "password": password, "name": "새 회원"}, nil); response.Code == http.StatusServiceUnavailable {
		t.Fatalf("sign-up still closed after lockdown ended: %s", response.Body.String())
	}
}

// 시연 모드에서는 v1 소셜 로그인도 처음 보는 신원으로 계정을 만들지 않고, 기존
// 계정은 관리자만 토큰을 받는다.
func TestDemoLockdownSocialLogin(t *testing.T) {
	admin, member := uuid.New(), uuid.New()
	cases := []struct {
		name    string
		user    uuid.UUID
		err     error
		wantErr error
	}{
		{name: "unknown identity", err: ErrAccountNotLinked, wantErr: ErrDemoLockdown},
		{name: "member", user: member, wantErr: ErrDemoLockdown},
		{name: "admin", user: admin},
	}
	for _, tc := range cases {
		t.Run("google "+tc.name, func(t *testing.T) {
			tokens := testTokenService(newMemoryRefreshStore())
			tokens.SetDemoLockdown(NewDemoLockdown([]uuid.UUID{admin}))
			accounts := &stubGoogleAccounts{user: googleLoginUser{ID: tc.user, Status: UserStatusActive}, err: tc.err}
			verifier := &stubGoogleVerifier{identity: GoogleIdentity{Subject: "google-subject", Email: "person@example.com", EmailVerified: true}}
			service, err := NewGoogleLoginService(verifier, accounts, tokens)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Login(context.Background(), "google-id-token", ClientInfo{}); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Login = %v, want %v", err, tc.wantErr)
			}
			if accounts.created {
				t.Fatal("v1 login must not create accounts during demo lockdown")
			}
		})
		t.Run("apple "+tc.name, func(t *testing.T) {
			tokens := testTokenService(newMemoryRefreshStore())
			tokens.SetDemoLockdown(NewDemoLockdown([]uuid.UUID{admin}))
			accounts := &stubAppleAccounts{user: appleLoginUser{ID: tc.user, Status: UserStatusActive}, err: tc.err}
			service, err := NewAppleLoginService(
				&stubAppleExchanger{response: AppleTokenResponse{IDToken: "apple-id-token"}},
				&stubAppleVerifier{identity: AppleIdentity{Subject: "apple-subject", Email: "person@example.com", EmailVerified: true}},
				accounts,
				tokens,
				&ProviderTokenCipher{key: []byte("0123456789abcdef0123456789abcdef")},
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Login(context.Background(), "single-use-code", "nonce", "", ClientInfo{}); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Login = %v, want %v", err, tc.wantErr)
			}
			if accounts.created {
				t.Fatal("v1 login must not create accounts during demo lockdown")
			}
		})
	}
}

func TestDemoLockdownGoogleLoginHTTP(t *testing.T) {
	tokens := testTokenService(newMemoryRefreshStore())
	tokens.SetDemoLockdown(NewDemoLockdown([]uuid.UUID{uuid.New()}))
	verifier := &stubGoogleVerifier{identity: GoogleIdentity{Subject: "google-subject", Email: "person@example.com", EmailVerified: true}}
	service, err := NewGoogleLoginService(verifier, &stubGoogleAccounts{err: ErrAccountNotLinked}, tokens)
	if err != nil {
		t.Fatal(err)
	}
	config, _ := NewTokenHTTPConfig(false, nil)
	handler := MountAuthHTTP(http.NotFoundHandler(), tokens, service, slog.New(slog.NewTextHandler(io.Discard, nil)), config)

	request := httptest.NewRequest(http.MethodPost, "/auth/google", bytes.NewBufferString(`{"id_token":"google-id-token"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertDemoLockdownResponse(t, response)
}
