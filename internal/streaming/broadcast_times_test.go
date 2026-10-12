package streaming

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func broadcastTimesProvider(t *testing.T, body string, status int) *YouTubeProvider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/liveBroadcasts" || r.URL.Query().Get("part") != "snippet" || r.URL.Query().Get("id") != "bid-1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	provider, err := NewYouTubeProvider(stubTokens{token: "at-value"}, newMemoryStore(), testCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	provider.apiBase = server.URL
	return provider
}

// TestYouTubeBroadcastTimes: 플랫폼이 기록한 실제 시작·종료 시각을 읽는다(#437).
func TestYouTubeBroadcastTimes(t *testing.T) {
	provider := broadcastTimesProvider(t, `{"items":[{"snippet":{"actualStartTime":"2026-10-12T08:00:05Z","actualEndTime":"2026-10-12T08:30:10Z"}}]}`, http.StatusOK)
	started, ended, err := provider.BroadcastTimes(context.Background(), uuid.New(), "bid-1")
	if err != nil {
		t.Fatal(err)
	}
	if started == nil || !started.Equal(time.Date(2026, 10, 12, 8, 0, 5, 0, time.UTC)) || ended == nil || !ended.Equal(time.Date(2026, 10, 12, 8, 30, 10, 0, time.UTC)) {
		t.Fatalf("times = %v / %v", started, ended)
	}
}

// TestYouTubeBroadcastTimesMissingOrFailed: 끝나지 않았거나 사라진 방송은 nil,
// 플랫폼 오류는 오류로 돌려준다.
func TestYouTubeBroadcastTimesMissingOrFailed(t *testing.T) {
	provider := broadcastTimesProvider(t, `{"items":[{"snippet":{"actualStartTime":"2026-10-12T08:00:05Z"}}]}`, http.StatusOK)
	if started, ended, err := provider.BroadcastTimes(context.Background(), uuid.New(), "bid-1"); err != nil || started == nil || ended != nil {
		t.Fatalf("live broadcast = %v / %v / %v, want start only", started, ended, err)
	}
	provider = broadcastTimesProvider(t, `{"items":[]}`, http.StatusOK)
	if started, ended, err := provider.BroadcastTimes(context.Background(), uuid.New(), "bid-1"); err != nil || started != nil || ended != nil {
		t.Fatalf("missing broadcast = %v / %v / %v, want nils", started, ended, err)
	}
	provider = broadcastTimesProvider(t, `{"error":{"code":500}}`, http.StatusInternalServerError)
	if _, _, err := provider.BroadcastTimes(context.Background(), uuid.New(), "bid-1"); err == nil {
		t.Fatal("platform error was not returned")
	}
}
