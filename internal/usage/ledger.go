package usage

import (
	"context"
	"fmt"
	"sort"
	"time"

	"inno-live-server/internal/plan"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// 방송 시간 원장(#274). 차감용 테이블을 따로 두지 않고 실사용 기록(송출·일시정지)
// 에서 조회할 때 계산한다 — 원본이 하나라 어긋날 일이 없다.
//
// 차감 규칙: 시점마다 "라이브이고 멈추지 않은 송출 대상 수" k로 유닛을 정하고
// (plan.Units: 720p k, FHD k+1) 시간을 곱해 더한다. 동시 송출에서 한쪽이 먼저
// 끝나거나 멈추면 그 뒤로는 단독 배수다. 슬롯 회계(#272)와 같은 규칙이다.

// LedgerLocation은 월 경계의 기준 시간대다(매월 1일 0시 KST).
var LedgerLocation = time.FixedZone("KST", 9*60*60)

// MonthRange는 t가 속한 달의 [시작, 끝)이다.
func MonthRange(t time.Time) (time.Time, time.Time) {
	local := t.In(LedgerLocation)
	start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, LedgerLocation)
	return start, start.AddDate(0, 1, 0)
}

// PauseSpan은 일시정지 구간이다. ResumedAt이 없으면 송출이 끝날 때(또는 지금)까지다.
type PauseSpan struct {
	PausedAt  time.Time
	ResumedAt *time.Time
}

// TargetTimeline은 송출 대상 하나의 라이브 구간이다.
type TargetTimeline struct {
	LiveAt  time.Time
	EndedAt *time.Time
	// Unclean은 종료 기록 없이 서버가 죽은 송출이다. 끝난 시각을 모르므로
	// 차감하지 않는다 — 사용자에게 불리하게 추정하지 않는다.
	Unclean bool
	Pauses  []PauseSpan
	// LegacyPausedSeconds는 구간 기록(#274) 이전 행의 일시정지 합계다. 구간이
	// 없을 때만 쓰며, 송출 끝에서 멈췄다고 보고 끝을 당긴다(근사).
	LegacyPausedSeconds float64
}

// ChargeSession은 한 세션의 [from, to) 안 방송 시간(onAir, 하나라도 송출 중인
// 시간)과 차감(charged, 유닛-시간)을 계산한다. 끝나지 않은 송출은 now까지 센다.
func ChargeSession(fhd bool, targets []TargetTimeline, from, to, now time.Time) (onAir, charged time.Duration) {
	type edge struct {
		at    time.Time
		delta int
	}
	var edges []edge
	for _, target := range targets {
		for _, span := range activeSpans(target, now) {
			start, end := maxTime(span[0], from), minTime(span[1], to)
			if end.After(start) {
				edges = append(edges, edge{start, +1}, edge{end, -1})
			}
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].at.Equal(edges[j].at) {
			return edges[i].delta < edges[j].delta // 같은 시각이면 끝을 먼저
		}
		return edges[i].at.Before(edges[j].at)
	})
	active := 0
	for i, e := range edges {
		if i > 0 && active > 0 {
			dt := e.at.Sub(edges[i-1].at)
			onAir += dt
			charged += dt * time.Duration(plan.Units(fhd, active))
		}
		active += e.delta
	}
	return onAir, charged
}

// activeSpans는 송출 하나의 라이브 구간에서 일시정지를 뺀 구간들이다.
func activeSpans(target TargetTimeline, now time.Time) [][2]time.Time {
	if target.Unclean {
		return nil
	}
	end := now
	if target.EndedAt != nil {
		end = *target.EndedAt
	}
	if len(target.Pauses) == 0 && target.LegacyPausedSeconds > 0 {
		end = end.Add(-time.Duration(target.LegacyPausedSeconds * float64(time.Second)))
	}
	if !end.After(target.LiveAt) {
		return nil
	}
	pauses := append([]PauseSpan(nil), target.Pauses...)
	sort.Slice(pauses, func(i, j int) bool { return pauses[i].PausedAt.Before(pauses[j].PausedAt) })
	var spans [][2]time.Time
	cursor := target.LiveAt
	for _, pause := range pauses {
		pauseEnd := end
		if pause.ResumedAt != nil {
			pauseEnd = *pause.ResumedAt
		}
		if pause.PausedAt.After(cursor) {
			spans = append(spans, [2]time.Time{cursor, minTime(pause.PausedAt, end)})
		}
		cursor = maxTime(cursor, pauseEnd)
		if !cursor.Before(end) {
			return spans
		}
	}
	return append(spans, [2]time.Time{cursor, end})
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// SessionCharge는 사용 내역의 한 줄(방송 한 번 = 세션 하나)이다.
type SessionCharge struct {
	SessionID  uuid.UUID
	StartedAt  time.Time
	Resolution string
	Providers  []string
	OnAir      time.Duration
	Charged    time.Duration
}

// Ledger는 사용자의 월 방송 시간을 계산한다.
type Ledger struct {
	db *gorm.DB
}

func NewLedger(db *gorm.DB) *Ledger {
	return &Ledger{db: db}
}

// Month는 userID의 [from, to) 안 방송들을 차감과 함께 돌려준다. 라이브로 가지
// 않은 세션(차감 0)은 빠진다. 해상도 기록이 없는 행(#271 이전·백필)은 720p로 본다.
func (l *Ledger) Month(ctx context.Context, userID uuid.UUID, from, to, now time.Time) ([]SessionCharge, error) {
	db := l.db.WithContext(ctx)
	var sessions []Session
	if err := db.Where("user_id = ? AND started_at < ? AND (ended_at IS NULL OR ended_at >= ?)", userID, to, from).
		Order("started_at").Find(&sessions).Error; err != nil {
		return nil, fmt.Errorf("read usage sessions: %w", err)
	}
	if len(sessions) == 0 {
		return nil, nil
	}
	sessionIDs := make([]uuid.UUID, len(sessions))
	for i, s := range sessions {
		sessionIDs[i] = s.ID
	}
	var broadcasts []Broadcast
	if err := db.Where("session_id IN ? AND live_at IS NOT NULL AND live_at < ?", sessionIDs, to).
		Find(&broadcasts).Error; err != nil {
		return nil, fmt.Errorf("read usage broadcasts: %w", err)
	}
	pausesByBroadcast := map[uuid.UUID][]PauseSpan{}
	if len(broadcasts) > 0 {
		broadcastIDs := make([]uuid.UUID, len(broadcasts))
		for i, b := range broadcasts {
			broadcastIDs[i] = b.ID
		}
		var pauses []Pause
		if err := db.Where("broadcast_id IN ?", broadcastIDs).Find(&pauses).Error; err != nil {
			return nil, fmt.Errorf("read usage pauses: %w", err)
		}
		for _, p := range pauses {
			pausesByBroadcast[p.BroadcastID] = append(pausesByBroadcast[p.BroadcastID], PauseSpan{PausedAt: p.PausedAt, ResumedAt: p.ResumedAt})
		}
	}
	bySession := map[uuid.UUID][]Broadcast{}
	for _, b := range broadcasts {
		bySession[b.SessionID] = append(bySession[b.SessionID], b)
	}

	var charges []SessionCharge
	for _, s := range sessions {
		resolution := "720p"
		if s.Resolution != nil {
			resolution = *s.Resolution
		}
		var timelines []TargetTimeline
		providers := []string{}
		seen := map[string]bool{}
		for _, b := range bySession[s.ID] {
			timelines = append(timelines, TargetTimeline{
				LiveAt:              *b.LiveAt,
				EndedAt:             b.EndedAt,
				Unclean:             b.EndedAt == nil && b.EndReason != nil,
				Pauses:              pausesByBroadcast[b.ID],
				LegacyPausedSeconds: b.PausedSeconds,
			})
			if !seen[b.Provider] {
				seen[b.Provider] = true
				providers = append(providers, b.Provider)
			}
		}
		onAir, charged := ChargeSession(resolution == "fhd", timelines, from, to, now)
		if charged <= 0 {
			continue
		}
		sort.Strings(providers)
		charges = append(charges, SessionCharge{SessionID: s.ID, StartedAt: s.StartedAt, Resolution: resolution, Providers: providers, OnAir: onAir, Charged: charged})
	}
	return charges, nil
}
