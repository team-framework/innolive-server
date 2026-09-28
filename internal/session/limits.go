package session

import (
	"time"

	"inno-live-server/internal/media"
)

// 방송 한도 집행(#275)이 세션에서 읽고 쓰는 것들이다. 판단은 서버 계층이 한다 —
// 원장(internal/usage)이 이 패키지에 의존하므로 여기서는 부를 수 없다.

// Notice는 클라이언트에 알릴 한도 사건이다. 세션 응답의 notices[]로 나간다.
type Notice struct {
	Code string    `json:"code"`
	At   time.Time `json:"at"`
}

// AddNotice는 알림을 한 번만 싣는다. 새로 실었으면 true다.
func (s *Session) AddNotice(code string, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, notice := range s.notices {
		if notice.Code == code {
			return false
		}
	}
	s.notices = append(s.notices, Notice{Code: code, At: at.UTC()})
	return true
}

// LastMediaFrameAt은 AI 처리까지 끝난 마지막 프레임 시각이다. 아직 없으면 zero다.
func (s *Session) LastMediaFrameAt() time.Time {
	unix := s.lastMediaFrameNano.Load()
	if unix == 0 {
		return time.Time{}
	}
	return time.Unix(0, unix)
}

// BroadcastActivity는 라이브인 송출 대상과 그중 멈추지 않은 수다. 모든 대상이
// 멈추면 AI 입력도 멈추므로, 입력 없음 판정은 멈추지 않은 대상이 있을 때만 한다.
func (s *Session) BroadcastActivity() (live []string, unpaused int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for provider, t := range s.targets {
		if t.phase != BroadcastPhaseLive || t.egress == nil || t.stopReason != nil {
			continue
		}
		phase := t.egress.Status().Phase
		if phase == media.EgressPhaseStopped {
			continue
		}
		live = append(live, provider)
		if phase != media.EgressPhasePaused && phase != media.EgressPhasePausedReconfiguring {
			unpaused++
		}
	}
	return live, unpaused
}
