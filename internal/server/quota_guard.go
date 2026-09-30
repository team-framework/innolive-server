package server

import (
	"slices"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"
)

// youtubeQuotaLow는 YouTube API 쿼터를 백그라운드 조회를 쉬어야 할 만큼 썼는지다
// (#361). 사용자가 쓰는 기능(직전 값 불러오기·채널 라이브 확인)은 끄지 않는다 —
// 원래 되던 기능이 안 되면 버그로 보이고, 각각 1~3유닛이라 아끼는 양도 작다.
// 사용자 눈에 보이지 않고 가장 많이 쓰는 주기 조회(스튜디오 종료 확인)만 쉰다.
func (s *Server) youtubeQuotaLow() bool {
	reporter, ok := s.streaming[auth.StreamingProviderYouTube].(streaming.QuotaReporter)
	return ok && reporter.Quota().Low()
}

// youtubeQuotaRatio는 일일 쿼터 대비 오늘 쓴 비율 추정치다.
func (s *Server) youtubeQuotaRatio() float64 {
	reporter, ok := s.streaming[auth.StreamingProviderYouTube].(streaming.QuotaReporter)
	if !ok {
		return 0
	}
	return reporter.Quota().Ratio()
}

// noticeYouTubeQuotaLow는 유튜브 쿼터가 80%를 넘었다는 알림이다(#366). 쿼터는
// 서비스 전체가 나눠 쓰므로, 방식 전환·새 방송처럼 쿼터를 크게 쓰는 동작이
// 실패하기 전에 알린다.
const noticeYouTubeQuotaLow = "youtube_quota_low"

// youtubeQuotaLowWarning은 방송 준비 응답에 싣는 경고다.
func youtubeQuotaLowWarning() streaming.Warning {
	return streaming.Warning{Code: noticeYouTubeQuotaLow, Message: "YouTube API usage for today is running low. Changing the broadcast mode or starting new broadcasts may fail until it resets at midnight Pacific Time."}
}

// noticeYouTubeQuotaLowFor는 쿼터가 모자랄 때 유튜브로 방송 중인 세션에 알림을 한
// 번 남긴다.
func (s *Server) noticeYouTubeQuotaLowFor(live *session.Session, targets []string, now time.Time) {
	if slices.Contains(targets, string(auth.StreamingProviderYouTube)) && s.youtubeQuotaLow() {
		live.AddNotice(noticeYouTubeQuotaLow, now)
	}
}

// publishYouTubeQuota는 쿼터 추정치를 메트릭으로 내고, 50·80·95%를 처음 넘을 때
// 로그를 남긴다. 한도 점검 루프가 부른다.
func (s *Server) publishYouTubeQuota() {
	reporter, ok := s.streaming[auth.StreamingProviderYouTube].(streaming.QuotaReporter)
	if !ok {
		return
	}
	used := reporter.Quota().Used()
	if s.metrics != nil {
		s.metrics.SetYouTubeQuotaUsed(used)
	}
	percent := used * 100 / streaming.YouTubeDailyQuota
	level := 0
	for _, threshold := range []int{50, 80, 95} {
		if percent >= threshold {
			level = threshold
		}
	}
	s.quotaLogMu.Lock()
	defer s.quotaLogMu.Unlock()
	if level < s.quotaLoggedLevel {
		s.quotaLoggedLevel = 0 // 태평양 시간 자정에 초기화됐다.
	}
	if level > s.quotaLoggedLevel {
		s.quotaLoggedLevel = level
		s.logger.Warn("youtube api quota usage high", "used_estimate", used, "daily_quota", streaming.YouTubeDailyQuota, "percent", percent)
	}
}
