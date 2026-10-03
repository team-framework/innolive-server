package server

import (
	"context"
	"sync"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"
)

const (
	// platformEndCheckInterval은 플랫폼 방송이 밖에서 끝났는지 묻는 주기다(#360).
	// 유튜브 쿼터는 프로젝트 전체가 하루 10,000이고 조회 1회가 1이다. 13세션이
	// 하루 종일 방송해도 3,744로 37%에 머문다(60초면 18,720로 초과).
	platformEndCheckInterval = 5 * time.Minute
	// platformEndCheckSlowInterval은 쿼터를 절반 넘게 썼을 때의 주기다(#366).
	platformEndCheckSlowInterval = 10 * time.Minute
	// stopReasonPlatformEnded는 사용자가 플랫폼 스튜디오에서 방송을 끝낸 경우다.
	stopReasonPlatformEnded = "platform_ended"
	noticePlatformEnded     = "platform_broadcast_ended"
)

// platformEndChecks는 대상별 마지막 확인 시각이다.
type platformEndChecks struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// due는 확인할 차례인지 보고, 차례면 시각을 기록한다. 오래된 항목은 치운다.
func (c *platformEndChecks) due(key string, now time.Time, interval time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[string]time.Time{}
	}
	for known, at := range c.last {
		if now.Sub(at) > time.Hour {
			delete(c.last, known)
		}
	}
	if at, ok := c.last[key]; ok && now.Sub(at) < interval {
		return false
	}
	c.last[key] = now
	return true
}

// stopPlatformEndedTargets는 플랫폼에서 이미 끝난 라이브 대상의 송출을 멈춘다. 멈추지
// 않으면 시청자가 없는 송출이 사용자의 방송 시간을 계속 차감한다. 확인에 실패하면
// 다음 차례에 다시 본다.
func (s *Server) stopPlatformEndedTargets(ctx context.Context, live *session.Session, targets []string, now time.Time) {
	// 쿼터가 모자라면 확인을 쉬고 남은 쿼터를 방송 시작·종료·전환에 남긴다(#361).
	if s.youtubeQuotaLow() {
		return
	}
	interval := platformEndCheckInterval
	if s.youtubeQuotaRatio() >= 0.5 {
		interval = platformEndCheckSlowInterval
	}
	for _, provider := range targets {
		checker, ok := s.streaming[auth.StreamingProvider(provider)].(streaming.BroadcastStatusChecker)
		if !ok {
			continue
		}
		broadcast, _ := live.PlatformBroadcast(provider)
		if broadcast.BroadcastID == "" || !s.platformEnds.due(live.ID+"/"+provider, now, interval) {
			continue
		}
		checkCtx, cancel := context.WithTimeout(auth.WithStreamingAccount(ctx, broadcast.AccountID), limitCheckInterval)
		ended, err := checker.BroadcastEnded(checkCtx, live.UserID, broadcast.BroadcastID)
		cancel()
		if err != nil {
			s.logger.Warn("platform broadcast status check failed", "session_id", live.ID, "provider", provider, "error", err)
			continue
		}
		if !ended {
			continue
		}
		_, stopped, phase, err := s.sessions.StopStreamWithReason(live.ID, stopReasonPlatformEnded, provider)
		if err != nil {
			s.logger.Warn("platform ended stop failed", "session_id", live.ID, "provider", provider, "error", err)
			continue
		}
		s.disposeBroadcast(live.UserID, stopped, phase)
		live.AddNotice(noticePlatformEnded, now)
		s.logger.Info("broadcast ended on platform; stopped egress", "session_id", live.ID, "provider", provider)
	}
}
