package session

import (
	"time"

	"github.com/google/uuid"
)

// UsageEventKind는 실사용 기록(#266)이 남기는 세션·송출 수명 사건이다.
type UsageEventKind string

const (
	UsageSessionStarted   UsageEventKind = "session_started"
	UsageSessionEnded     UsageEventKind = "session_ended"
	UsageBroadcastStarted UsageEventKind = "broadcast_started"
	UsageBroadcastLive    UsageEventKind = "broadcast_live"
	UsageBroadcastPaused  UsageEventKind = "broadcast_paused"
	UsageBroadcastResumed UsageEventKind = "broadcast_resumed"
	UsageBroadcastEnded   UsageEventKind = "broadcast_ended"
)

// UsageEvent는 사건 하나다. 종류마다 쓰는 필드만 채운다.
type UsageEvent struct {
	Kind         UsageEventKind
	At           time.Time
	SessionID    string
	BroadcastID  string
	UserID       uuid.UUID
	AIProcessing string
	Provider     string
	Reason       string
}

// UsageRecorder는 사건을 저장소로 넘긴다. 세션 잠금을 쥔 경로에서도 부르므로
// 구현은 절대 블록하지 않아야 한다 — 기록 실패가 방송을 막으면 안 된다.
// 세션 계층은 DB를 모르므로 서버가 조립 시점에 주입한다.
type UsageRecorder interface {
	RecordUsage(UsageEvent)
}

// SetUsageRecorder는 서버 조립 직후 한 번만 호출한다. nil이면 기록하지 않는다.
func (m *Manager) SetUsageRecorder(recorder UsageRecorder) { m.usage = recorder }

func (m *Manager) recordUsage(event UsageEvent) {
	if m.usage == nil {
		return
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	m.usage.RecordUsage(event)
}

func (m *Manager) recordSessionEnded(s *Session, reason string) {
	m.recordUsage(UsageEvent{Kind: UsageSessionEnded, SessionID: s.ID, Reason: reason})
}
