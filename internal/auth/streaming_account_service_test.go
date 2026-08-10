package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// orderRecordingStore는 Disconnect의 3단계 순서를 기록하기 위한 store 래퍼다.
type orderRecordingStore struct {
	StreamingAccountStore
	mu    *sync.Mutex
	order *[]string
}

func (s orderRecordingStore) Delete(ctx context.Context, id uuid.UUID) error {
	s.mu.Lock()
	*s.order = append(*s.order, "delete")
	s.mu.Unlock()
	return s.StreamingAccountStore.Delete(ctx, id)
}

func disconnectFixture(t *testing.T, cleanupErr, revokeErr error) (*StreamingAccountService, *memoryStreamingAccountStore, uuid.UUID, *[]string) {
	t.Helper()
	cipher := testProviderTokenCipher(t)
	ciphertext, version, err := cipher.Encrypt("rt-secret")
	if err != nil {
		t.Fatal(err)
	}
	store := newMemoryStreamingAccountStore()
	userID := uuid.New()
	if err := store.Upsert(context.Background(), StreamingAccount{
		UserID:                 userID,
		Provider:               StreamingProviderYouTube,
		ChannelID:              "UCabc",
		RefreshTokenCiphertext: ciphertext,
		TokenKeyVersion:        version,
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	order := []string{}
	hooks := map[StreamingProvider]StreamingDisconnectHooks{
		StreamingProviderYouTube: {
			CleanupResources: func(context.Context, StreamingAccount) error {
				mu.Lock()
				order = append(order, "cleanup")
				mu.Unlock()
				return cleanupErr
			},
			RevokeToken: func(_ context.Context, refreshToken string) error {
				mu.Lock()
				order = append(order, "revoke:"+refreshToken)
				mu.Unlock()
				return revokeErr
			},
		},
	}
	service, err := NewStreamingAccountService(
		orderRecordingStore{StreamingAccountStore: store, mu: &mu, order: &order},
		testUserStatusChecker{status: UserStatusActive},
		cipher,
		hooks,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return service, store, userID, &order
}

// TestDisconnectRunsStepsInOrder: ①리소스 삭제 → ②권한 취소(복호화된 RT 전달)
// → ③행 삭제 순서가 지켜져야 한다(#88 — 토큰을 먼저 폐기하면 ①이 불가능).
func TestDisconnectRunsStepsInOrder(t *testing.T) {
	service, store, userID, order := disconnectFixture(t, nil, nil)
	if err := service.Disconnect(context.Background(), userID, StreamingProviderYouTube); err != nil {
		t.Fatal(err)
	}
	want := []string{"cleanup", "revoke:rt-secret", "delete"}
	if len(*order) != len(want) {
		t.Fatalf("order = %v, want %v", *order, want)
	}
	for i := range want {
		if (*order)[i] != want[i] {
			t.Fatalf("order = %v, want %v", *order, want)
		}
	}
	if _, err := store.Get(context.Background(), userID, StreamingProviderYouTube); !errors.Is(err, ErrStreamingAccountNotFound) {
		t.Fatal("account row must be deleted")
	}
}

// TestDisconnectContinuesWhenPlatformStepsFail: ①·②가 실패해도 ③(행 삭제)은
// 수행돼야 한다 — 이미 토큰이 무효화된 연결을 해제하는 것이 정상 시나리오다.
func TestDisconnectContinuesWhenPlatformStepsFail(t *testing.T) {
	cases := []struct {
		name       string
		cleanupErr error
		revokeErr  error
	}{
		{"cleanup fails", errors.New("live api down"), nil},
		{"revoke fails", nil, errors.New("revoke endpoint down")},
		{"both fail", errors.New("live api down"), errors.New("revoke endpoint down")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, store, userID, order := disconnectFixture(t, tc.cleanupErr, tc.revokeErr)
			if err := service.Disconnect(context.Background(), userID, StreamingProviderYouTube); err != nil {
				t.Fatalf("Disconnect must succeed despite platform failures: %v", err)
			}
			if _, err := store.Get(context.Background(), userID, StreamingProviderYouTube); !errors.Is(err, ErrStreamingAccountNotFound) {
				t.Fatal("account row must be deleted even when platform steps fail")
			}
			if (*order)[len(*order)-1] != "delete" {
				t.Fatalf("order = %v, want delete last", *order)
			}
		})
	}
}

// TestDisconnectWithoutHooksDeletesRow: 훅이 없는 플랫폼(정리할 것이 없는
// 경우)은 행 삭제만으로 해제된다.
func TestDisconnectWithoutHooksDeletesRow(t *testing.T) {
	store := newMemoryStreamingAccountStore()
	userID := uuid.New()
	if err := store.Upsert(context.Background(), StreamingAccount{
		UserID:    userID,
		Provider:  StreamingProviderYouTube,
		ChannelID: "UCabc",
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewStreamingAccountService(store, testUserStatusChecker{status: UserStatusActive}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Disconnect(context.Background(), userID, StreamingProviderYouTube); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), userID, StreamingProviderYouTube); !errors.Is(err, ErrStreamingAccountNotFound) {
		t.Fatal("account row must be deleted")
	}
}

func TestDisconnectNotConnected(t *testing.T) {
	service, _, _, _ := disconnectFixture(t, nil, nil)
	if err := service.Disconnect(context.Background(), uuid.New(), StreamingProviderYouTube); !errors.Is(err, ErrStreamingAccountNotFound) {
		t.Fatalf("error = %v, want ErrStreamingAccountNotFound", err)
	}
}

// TestRevokeTokenTreatsAlreadyInvalidAsSuccess: 이미 무효한 토큰의 revoke는
// Google이 400을 주지만, 해제 관점에선 목적 달성이므로 에러가 아니어야 한다.
func TestRevokeTokenTreatsAlreadyInvalidAsSuccess(t *testing.T) {
	var received string
	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		received = r.PostForm.Get("token")
		w.WriteHeader(http.StatusOK)
	}))
	defer okServer.Close()
	invalidServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer invalidServer.Close()
	brokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer brokenServer.Close()

	client, err := NewYouTubeOAuthClient(YouTubeOAuthConfig{ClientID: "id", ClientSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	client.revokeURL = okServer.URL
	if err := client.RevokeToken(context.Background(), "rt-value"); err != nil {
		t.Fatal(err)
	}
	if received != "rt-value" {
		t.Fatalf("revoked token = %q", received)
	}
	client.revokeURL = invalidServer.URL
	if err := client.RevokeToken(context.Background(), "already-dead"); err != nil {
		t.Fatalf("400 must not be an error: %v", err)
	}
	client.revokeURL = brokenServer.URL
	if err := client.RevokeToken(context.Background(), "any"); err == nil {
		t.Fatal("5xx must be an error")
	}
}
