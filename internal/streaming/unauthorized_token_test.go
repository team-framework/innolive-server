package streaming

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// invalidatingTokens는 버리라고 알린 access token을 기록한다.
type invalidatingTokens struct {
	token       string
	mu          sync.Mutex
	invalidated []string
}

func (s *invalidatingTokens) AccessToken(context.Context, uuid.UUID) (string, error) {
	return s.token, nil
}

func (s *invalidatingTokens) InvalidateAccessToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated = append(s.invalidated, token)
}

func (s *invalidatingTokens) dropped() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.invalidated...)
}

// 플랫폼이 401을 주면 캐시된 access token이 죽은 것이다(권한 취소·회전). 버리지
// 않으면 만료(최대 하루)까지 모든 호출이 같은 토큰으로 계속 실패한다(P1-15).
func TestYouTubeUnauthorizedDropsCachedAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Invalid Credentials"}}`))
	}))
	t.Cleanup(server.Close)
	tokens := &invalidatingTokens{token: "stale-token"}
	provider, err := NewYouTubeProvider(tokens, newMemoryStore(), testCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	provider.apiBase = server.URL
	if err := provider.Stop(context.Background(), uuid.New(), PreparedBroadcast{BroadcastID: "broadcast-1"}); err == nil {
		t.Fatal("Stop succeeded against a 401")
	}
	if got := tokens.dropped(); len(got) != 1 || got[0] != "stale-token" {
		t.Fatalf("invalidated tokens = %v, want the stale token dropped", got)
	}
}

func TestChzzkUnauthorizedDropsCachedAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":401,"message":"INVALID_TOKEN"}`))
	}))
	t.Cleanup(server.Close)
	tokens := &invalidatingTokens{token: "stale-token"}
	provider, err := NewChzzkProvider(tokens)
	if err != nil {
		t.Fatal(err)
	}
	provider.apiBase = server.URL
	provider.httpClient = server.Client()
	if _, err := provider.Defaults(context.Background(), uuid.New()); err == nil {
		t.Fatal("Defaults succeeded against a 401")
	}
	if got := tokens.dropped(); len(got) != 1 || got[0] != "stale-token" {
		t.Fatalf("invalidated tokens = %v, want the stale token dropped", got)
	}
}
