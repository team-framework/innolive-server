package server

import (
	"context"
	"testing"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
)

// stallingConnectionProvider는 연결 확인(토큰 갱신)이 응답하지 않는 플랫폼이다.
type stallingConnectionProvider struct {
	*stubStreamingProvider
}

func (stallingConnectionProvider) Connected(ctx context.Context, _ uuid.UUID) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

// 한도 집행 루프는 모든 세션을 한 고루틴에서 돈다. 화질 제안을 위한 연결 확인이
// 토큰 갱신에서 멈추면 루프 전체가 멈춰 한도 집행도 같이 멈춘다(P1-29).
func TestUpgradeConnectionCheckIsBounded(t *testing.T) {
	previous := upgradeConnectionCheckTimeout
	upgradeConnectionCheckTimeout = 50 * time.Millisecond
	t.Cleanup(func() { upgradeConnectionCheckTimeout = previous })

	_, manager, application := newStreamTestApplicationWithServer(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: stallingConnectionProvider{&stubStreamingProvider{}},
	})
	live, _, err := manager.CreateForUser(uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan []string, 1)
	go func() { done <- application.addableProviders(context.Background(), live, []string{"chzzk"}) }()
	select {
	case addable := <-done:
		if len(addable) != 0 {
			t.Fatalf("addable = %v, want none when the check timed out", addable)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection check blocked the enforcement loop")
	}
}
