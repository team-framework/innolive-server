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

// MediaIdleSince는 입력 없음 시계의 시작점이다 — 마지막 처리 프레임과, 송출 시작·
// 재개 중 늦은 쪽이다. 모두 멈춘 동안에는 AI 입력이 멈춰 프레임이 없으므로, 재개
// 시각에서 다시 세지 않으면 오래 멈췄다 재개한 직후 곧바로 입력 없음이 된다.
// 둘 다 없으면 zero다.
func (s *Session) MediaIdleSince() time.Time {
	latest := max(s.lastMediaFrameNano.Load(), s.mediaIdleResetNano.Load())
	if latest == 0 {
		return time.Time{}
	}
	return time.Unix(0, latest)
}

// restartMediaIdleClock은 송출 시작·재개에서 입력 없음 시계를 다시 시작한다.
func (s *Session) restartMediaIdleClock(at time.Time) {
	s.mediaIdleResetNano.Store(at.UnixNano())
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
