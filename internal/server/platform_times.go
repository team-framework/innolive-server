package server

import (
	"context"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
)

// PlatformTimesRecorder는 송출 세대에 플랫폼의 실제 방송 시각을 남긴다(#437).
// 프로덕션은 usage.PlatformTimesStore다.
type PlatformTimesRecorder interface {
	RecordPlatformTimes(ctx context.Context, usageID, platformBroadcastID string, started, ended *time.Time) error
}

// platformTimesDelay는 직접 송출이 끝난 뒤 플랫폼 시각을 읽기까지 기다리는 시간이다.
// 종료 전환(EndLive)과 비동기 실사용 기록이 먼저 끝나야 종료 시각과 대조할 행이
// 생긴다. 테스트가 줄인다.
var platformTimesDelay = 30 * time.Second

// SetPlatformTimesStore는 직접 송출 종료 시 플랫폼 시각 대조를 붙인다(#437).
// 서버 조립 단계에서 한 번만 호출한다.
func (s *Server) SetPlatformTimesStore(store PlatformTimesRecorder) {
	if store == nil || s.sessions == nil {
		return
	}
	s.platformTimes = store
	s.sessions.SetDirectBroadcastEnded(s.reconcileDirectBroadcast)
}

// reconcileDirectBroadcast는 끝난 직접 송출의 플랫폼 실제 시각을 읽어 기록한다.
// 대조는 보조 기록이라 실패해도 방송·차감에 영향을 주지 않고 로그만 남긴다.
// 대기와 플랫폼 왕복 모두 상한이 있어 고루틴은 스스로 끝난다.
func (s *Server) reconcileDirectBroadcast(userID uuid.UUID, usageID string, broadcast session.PlatformBroadcast) {
	providerName := auth.StreamingProvider(broadcast.Provider)
	reader, ok := s.streaming[providerName].(streaming.BroadcastTimesReader)
	if !ok {
		// 치지직은 방송 객체가 없어 대조할 시각이 없다.
		return
	}
	if providerName == auth.StreamingProviderYouTube && s.youtubeQuotaLow() {
		s.logger.Info("platform times skipped: youtube quota low", "usage_broadcast_id", usageID)
		return
	}
	time.Sleep(platformTimesDelay)
	ctx, cancel := context.WithTimeout(auth.WithStreamingAccount(context.Background(), broadcast.AccountID), platformCleanupTimeout)
	defer cancel()
	started, ended, err := reader.BroadcastTimes(ctx, userID, broadcast.BroadcastID)
	if err != nil {
		s.logger.Warn("read platform broadcast times failed", "usage_broadcast_id", usageID, "broadcast_id", broadcast.BroadcastID, "error", err)
		return
	}
	if err := s.platformTimes.RecordPlatformTimes(ctx, usageID, broadcast.BroadcastID, started, ended); err != nil {
		s.logger.Warn("record platform broadcast times failed", "usage_broadcast_id", usageID, "broadcast_id", broadcast.BroadcastID, "error", err)
	}
}
