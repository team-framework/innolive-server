package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func youTubeAccountWithToken(t *testing.T, store *memoryStreamingAccountStore, cipher *ProviderTokenCipher, refresh string) uuid.UUID {
	t.Helper()
	ciphertext, version, err := cipher.Encrypt(refresh)
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.New()
	if err := store.Upsert(context.Background(), StreamingAccount{
		UserID: userID, Provider: StreamingProviderYouTube, ChannelID: "UCabc",
		RefreshTokenCiphertext: ciphertext, TokenKeyVersion: version,
	}); err != nil {
		t.Fatal(err)
	}
	return userID
}

// Google 토큰 엔드포인트의 4xx 중 invalid_grant만 refresh token이 죽었다는 뜻이다.
// invalid_client 같은 서버 설정 오류까지 재연결 필요로 표시하면 모든 사용자의 연결이
// 망가진 것처럼 보인다(P1-14).
func TestYouTubeTokenEndpointDistinguishesInvalidGrant(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		invalid bool
	}{
		{"invalid grant", http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`, true},
		{"invalid client", http.StatusUnauthorized, `{"error":"invalid_client"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := NewYouTubeOAuthClient(YouTubeOAuthConfig{ClientID: "id", ClientSecret: "secret"})
			if err != nil {
				t.Fatal(err)
			}
			client.tokenURL = server.URL
			_, err = client.RefreshAccessToken(context.Background(), "rt")
			if got := errors.Is(err, ErrYouTubeRefreshTokenInvalid); got != tc.invalid {
				t.Fatalf("errors.Is(%v, ErrYouTubeRefreshTokenInvalid) = %v, want %v", err, got, tc.invalid)
			}
		})
	}
}

func TestYouTubeAccessTokenDoesNotMarkReconnectForOtherRejections(t *testing.T) {
	cipher := testProviderTokenCipher(t)
	store := newMemoryStreamingAccountStore()
	userID := youTubeAccountWithToken(t, store, cipher, "rt-valid")
	provider, err := NewYouTubeAccessTokenProvider(&stubYouTubeAuthorizer{refreshErr: ErrYouTubeAuthCodeRejected}, store, cipher)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.AccessToken(context.Background(), userID); err == nil || errors.Is(err, ErrStreamingReconnectRequired) {
		t.Fatalf("error = %v, want a plain failure", err)
	}
	account, _ := store.Get(context.Background(), userID, StreamingProviderYouTube)
	if account.ReconnectRequiredAt != nil {
		t.Fatal("a non-invalid_grant rejection marked the connection as needing reconnect")
	}
}

// 갱신에 성공했는데 재연결 필요 표식이 남으면 연결 목록이 계속 "재연결 필요"로
// 보인다(P1-14).
func TestYouTubeAccessTokenClearsReconnectMarkOnSuccess(t *testing.T) {
	cipher := testProviderTokenCipher(t)
	store := newMemoryStreamingAccountStore()
	userID := youTubeAccountWithToken(t, store, cipher, "rt-valid")
	account, _ := store.Get(context.Background(), userID, StreamingProviderYouTube)
	if err := store.MarkReconnectRequired(context.Background(), account.ID, account.RefreshTokenCiphertext, time.Now()); err != nil {
		t.Fatal(err)
	}
	provider, err := NewYouTubeAccessTokenProvider(&stubYouTubeAuthorizer{refreshToken: YouTubeTokenResponse{AccessToken: "at", ExpiresIn: 3600}}, store, cipher)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.AccessToken(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	account, _ = store.Get(context.Background(), userID, StreamingProviderYouTube)
	if account.ReconnectRequiredAt != nil {
		t.Fatal("reconnect mark survived a successful refresh")
	}
}

// fakeChzzkRefresher는 refresh 호출만 흉내 낸다.
type fakeChzzkRefresher struct {
	ChzzkAuthorizer
	refresh func(ctx context.Context, refreshToken string) (ChzzkTokenResponse, error)
}

func (f fakeChzzkRefresher) RefreshAccessToken(ctx context.Context, refreshToken string) (ChzzkTokenResponse, error) {
	return f.refresh(ctx, refreshToken)
}

func chzzkAccountWithToken(t *testing.T, store *memoryStreamingAccountStore, cipher *ProviderTokenCipher, refresh string) uuid.UUID {
	t.Helper()
	ciphertext, version, err := cipher.Encrypt(refresh)
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.New()
	if err := store.Upsert(context.Background(), StreamingAccount{
		UserID: userID, Provider: StreamingProviderChzzk, ChannelID: "channel-1",
		RefreshTokenCiphertext: ciphertext, TokenKeyVersion: version,
	}); err != nil {
		t.Fatal(err)
	}
	return userID
}

func storedChzzkRefresh(t *testing.T, store *memoryStreamingAccountStore, cipher *ProviderTokenCipher, userID uuid.UUID) string {
	t.Helper()
	account, err := store.Get(context.Background(), userID, StreamingProviderChzzk)
	if err != nil {
		t.Fatal(err)
	}
	value, err := cipher.Decrypt(account.RefreshTokenCiphertext, account.TokenKeyVersion)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// 치지직 refresh token은 일회용이다. 플랫폼이 회전한 뒤 요청이 취소돼 새 토큰을
// 저장하지 못하면 옛 토큰은 이미 소비돼 연결이 영구히 끊긴다(P1-12).
func TestChzzkRotationSurvivesRequestCancellation(t *testing.T) {
	cipher := testProviderTokenCipher(t)
	store := newMemoryStreamingAccountStore()
	userID := chzzkAccountWithToken(t, store, cipher, "rt-1")
	entered, release := make(chan struct{}), make(chan struct{})
	provider, err := NewChzzkAccessTokenProvider(fakeChzzkRefresher{refresh: func(ctx context.Context, _ string) (ChzzkTokenResponse, error) {
		close(entered)
		<-release
		// 플랫폼은 이미 회전했다. 호출자가 취소됐으면 응답을 받지 못한다.
		if err := ctx.Err(); err != nil {
			return ChzzkTokenResponse{}, err
		}
		return ChzzkTokenResponse{AccessToken: "at", RefreshToken: "rt-2", ExpiresIn: 86400}, nil
	}}, store, cipher)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := provider.AccessToken(ctx, userID)
		done <- err
	}()
	<-entered
	cancel()
	close(release)
	<-done
	if got := storedChzzkRefresh(t, store, cipher, userID); got != "rt-2" {
		t.Fatalf("stored refresh = %q after a cancelled request, want the rotated rt-2", got)
	}
}

// 이관 중에는 두 슬롯이 같은 토큰으로 회전할 수 있다. 늦은 쪽은 거절되는데, 그때
// 재연결 필요로 표시하면 다른 슬롯이 방금 회전한 정상 연결이 망가진다(P1-13).
func TestChzzkRejectedRotationRetriesWithTokenRotatedElsewhere(t *testing.T) {
	cipher := testProviderTokenCipher(t)
	store := newMemoryStreamingAccountStore()
	userID := chzzkAccountWithToken(t, store, cipher, "rt-1")
	account, _ := store.Get(context.Background(), userID, StreamingProviderChzzk)
	var mu sync.Mutex
	var used []string
	provider, err := NewChzzkAccessTokenProvider(fakeChzzkRefresher{refresh: func(_ context.Context, refreshToken string) (ChzzkTokenResponse, error) {
		mu.Lock()
		used = append(used, refreshToken)
		mu.Unlock()
		if refreshToken == "rt-1" {
			// 다른 슬롯이 rt-1을 먼저 회전해 rt-2를 저장했다.
			ciphertext, version, _ := cipher.Encrypt("rt-2")
			if err := store.UpdateRefreshToken(context.Background(), account.ID, account.RefreshTokenCiphertext, ciphertext, version, nil); err != nil {
				t.Error(err)
			}
			return ChzzkTokenResponse{}, ErrChzzkAuthCodeRejected
		}
		return ChzzkTokenResponse{AccessToken: "at", RefreshToken: "rt-3", ExpiresIn: 86400}, nil
	}}, store, cipher)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.AccessToken(context.Background(), userID); err != nil {
		t.Fatalf("access token after a concurrent rotation = %v", err)
	}
	refreshed, _ := store.Get(context.Background(), userID, StreamingProviderChzzk)
	if refreshed.ReconnectRequiredAt != nil {
		t.Fatal("connection marked for reconnect although another slot had rotated it")
	}
	if got := storedChzzkRefresh(t, store, cipher, userID); got != "rt-3" {
		t.Fatalf("stored refresh = %q, want rt-3", got)
	}
	if len(used) != 2 || used[1] != "rt-2" {
		t.Fatalf("refresh calls used %v, want rt-1 then rt-2", used)
	}
}

// 거절된 access token을 버리면 다음 호출은 캐시 대신 refresh token으로 새로 받는다
// (P1-15). 다른 값의 토큰은 건드리지 않는다.
func TestYouTubeInvalidateAccessTokenForcesRefresh(t *testing.T) {
	cipher := testProviderTokenCipher(t)
	store := newMemoryStreamingAccountStore()
	userID := youTubeAccountWithToken(t, store, cipher, "rt-valid")
	oauth := &stubYouTubeAuthorizer{refreshToken: YouTubeTokenResponse{AccessToken: "at-1", ExpiresIn: 3600}}
	provider, err := NewYouTubeAccessTokenProvider(oauth, store, cipher)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.AccessToken(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	provider.InvalidateAccessToken("some-other-token")
	if _, err := provider.AccessToken(context.Background(), userID); err != nil || oauth.refreshCalls.Load() != 1 {
		t.Fatalf("unrelated invalidation forced a refresh (calls %d, err %v)", oauth.refreshCalls.Load(), err)
	}
	provider.InvalidateAccessToken("at-1")
	if _, err := provider.AccessToken(context.Background(), userID); err != nil || oauth.refreshCalls.Load() != 2 {
		t.Fatalf("refresh calls after invalidation = %d (err %v), want 2", oauth.refreshCalls.Load(), err)
	}
}
