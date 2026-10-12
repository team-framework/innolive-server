package session

import (
	"sort"
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

// SetBroadcastRemaining은 "현재 방식으로 더 방송할 수 있는 시간"을 싣는다(#276).
// 한도 점검 루프가 방송 중에 주기마다 갱신한다. 방송이 없을 때는 차감이 없으므로
// 마지막 값을 유지한다(#394). nil은 무제한이거나 아직 계산하지 않음이다.
func (s *Session) SetBroadcastRemaining(remaining *time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if remaining == nil {
		s.broadcastRemaining = nil
		return
	}
	seconds := int64(remaining.Seconds())
	s.broadcastRemaining = &seconds
}

// HasBroadcastRemaining은 남은 시간이 한 번이라도 계산됐는지다.
func (s *Session) HasBroadcastRemaining() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.broadcastRemaining != nil
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

// DirectLiveTargets는 온디바이스 세션에서 앱이 직접 송출 중인(egress 없는)
// 라이브 대상이다(#434). BroadcastActivity는 egress가 있는 대상만 센다.
func (s *Session) DirectLiveTargets() []string {
	if s.AIProcessing != AIProcessingOnDevice {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var live []string
	for provider, t := range s.targets {
		if t.phase == BroadcastPhaseLive && t.egress == nil {
			live = append(live, provider)
		}
	}
	return live
}

// MarkHeartbeat는 앱이 직접 송출 중임을 알린 시각을 남긴다(#436). 연결된 대상이
// 없는 보고는 부르지 않는다 — 끊긴 채 보고만 오는 상태도 입력 없음이다.
func (s *Session) MarkHeartbeat(at time.Time) {
	s.heartbeatNano.Store(at.UnixNano())
}

// HeartbeatIdleSince는 직접 송출의 입력 없음 시계가 시작된 시각이다. 마지막
// 하트비트와 라이브 전환 중 늦은 쪽이다.
func (s *Session) HeartbeatIdleSince() time.Time {
	latest := s.heartbeatNano.Load()
	if latest == 0 {
		return time.Time{}
	}
	return time.Unix(0, latest)
}

// TakeDirectStops는 서버가 끝낸 직접 송출 대상을 넘기고 비운다(#436). 앱은 이
// 대상의 연결을 끊는다 — 치지직은 앱이 끊어야 방송이 끝난다.
func (s *Session) TakeDirectStops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.directStops) == 0 {
		return nil
	}
	stops := make([]string, 0, len(s.directStops))
	for provider := range s.directStops {
		stops = append(stops, provider)
	}
	sort.Strings(stops)
	s.directStops = nil
	return stops
}

// HasDirectStops는 앱에 아직 알리지 않은 종료 대상이 있는지다.
func (s *Session) HasDirectStops() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.directStops) > 0
}
