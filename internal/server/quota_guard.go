package server

import (
	"inno-live-server/internal/auth"
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
