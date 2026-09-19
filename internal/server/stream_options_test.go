package server

import (
	"testing"
	"time"

	"inno-live-server/internal/auth"
)

// TestStreamOptionsForChzzkShortensReconnectBudget: 치지직은 RTMP 재연결이 곧
// 새 방송이라 종료 유예(실측 13~14초)보다 짧은 예산을 써야 한다.
func TestStreamOptionsForChzzkShortensReconnectBudget(t *testing.T) {
	options := streamOptionsFor(auth.StreamingProviderChzzk)
	if options.ReconnectMaxElapsed != chzzkEgressReconnectMaxElapsed {
		t.Fatalf("budget = %v, want %v", options.ReconnectMaxElapsed, chzzkEgressReconnectMaxElapsed)
	}
	// 실측 유예의 최솟값은 13초였다. 예산이 그 이상이면 유예가 지난 뒤
	// 재연결에 성공해 시청자에게 방송이 하나 더 생긴다.
	if chzzkEgressReconnectMaxElapsed >= 13*time.Second {
		t.Fatalf("budget %v must stay below the measured 13s grace", chzzkEgressReconnectMaxElapsed)
	}
}

// TestStreamOptionsForYouTubeKeepsDefault: 유튜브는 같은 방송 객체에 다시
// 붙으므로 종전 90초 예산을 그대로 쓴다. 0은 "지정하지 않음"이다.
func TestStreamOptionsForYouTubeKeepsDefault(t *testing.T) {
	if options := streamOptionsFor(auth.StreamingProviderYouTube); options.ReconnectMaxElapsed != 0 {
		t.Fatalf("YouTube budget = %v, want the media default", options.ReconnectMaxElapsed)
	}
}
