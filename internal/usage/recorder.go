package usage

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"inno-live-server/internal/session"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	eventBuffer  = 256
	writeTimeout = 5 * time.Second
)

// Recorder는 session.UsageRecorder 구현이다. 사건을 버퍼에 넣고 워커 하나가
// 순서대로 DB에 쓴다 — 세션 잠금 경로를 DB 왕복으로 붙잡지 않고, 시작이 종료보다
// 먼저 쓰이는 순서도 지킨다. 버퍼가 차면 사건을 버린다(방송이 우선).
type Recorder struct {
	db     *gorm.DB
	logger *slog.Logger
	events chan session.UsageEvent
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
	// pausedAt은 일시정지 중인 송출의 시작 시각이다. 워커만 만진다.
	pausedAt map[string]time.Time
}

func NewRecorder(db *gorm.DB, logger *slog.Logger) *Recorder {
	r := &Recorder{
		db:       db,
		logger:   logger,
		events:   make(chan session.UsageEvent, eventBuffer),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		pausedAt: make(map[string]time.Time),
	}
	go r.run()
	return r
}

func (r *Recorder) RecordUsage(event session.UsageEvent) {
	select {
	case r.events <- event:
	default:
		r.logger.Warn("usage event dropped; buffer full", "kind", event.Kind, "session_id", event.SessionID)
	}
}

// Close는 버퍼에 남은 사건을 모두 쓰고 워커를 끝낸다. 세션 teardown이 끝난 뒤,
// DB 연결을 닫기 전에 부른다.
func (r *Recorder) Close(ctx context.Context) error {
	r.once.Do(func() { close(r.stop) })
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("drain usage events: %w", ctx.Err())
	}
}

func (r *Recorder) run() {
	defer close(r.done)
	for {
		select {
		case event := <-r.events:
			r.write(event)
		case <-r.stop:
			for {
				select {
				case event := <-r.events:
					r.write(event)
				default:
					return
				}
			}
		}
	}
}

func (r *Recorder) write(event session.UsageEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if err := r.apply(ctx, event); err != nil {
		r.logger.Warn("record usage event failed", "kind", event.Kind, "session_id", event.SessionID,
			"broadcast_id", event.BroadcastID, "error", err)
	}
}

func (r *Recorder) apply(ctx context.Context, event session.UsageEvent) error {
	db := r.db.WithContext(ctx)
	switch event.Kind {
	case session.UsageSessionStarted:
		sessionID, err := uuid.Parse(event.SessionID)
		if err != nil {
			return fmt.Errorf("parse session id: %w", err)
		}
		row := Session{ID: sessionID, IsGuest: event.UserID == uuid.Nil, StartedAt: event.At, Source: SourceLive}
		if event.UserID != uuid.Nil {
			row.UserID = &event.UserID
		}
		if event.AIProcessing != "" {
			row.AIProcessing = &event.AIProcessing
		}
		return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error

	case session.UsageSessionEnded:
		return db.Model(&Session{}).Where("session_id = ? AND ended_at IS NULL", event.SessionID).
			Updates(map[string]any{"ended_at": event.At, "end_reason": event.Reason}).Error

	case session.UsageBroadcastStarted:
		id, err := uuid.Parse(event.BroadcastID)
		if err != nil {
			return fmt.Errorf("parse broadcast id: %w", err)
		}
		sessionID, err := uuid.Parse(event.SessionID)
		if err != nil {
			return fmt.Errorf("parse session id: %w", err)
		}
		row := Broadcast{ID: id, SessionID: sessionID, Provider: event.Provider, StartedAt: event.At, Source: SourceLive}
		return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error

	case session.UsageBroadcastLive:
		return db.Model(&Broadcast{}).Where("id = ? AND live_at IS NULL", event.BroadcastID).
			Update("live_at", event.At).Error

	case session.UsageBroadcastPaused:
		if _, paused := r.pausedAt[event.BroadcastID]; !paused {
			r.pausedAt[event.BroadcastID] = event.At
		}
		return nil

	case session.UsageBroadcastResumed:
		return db.Model(&Broadcast{}).Where("id = ?", event.BroadcastID).
			Update("paused_seconds", gorm.Expr("paused_seconds + ?", r.takePaused(event))).Error

	case session.UsageBroadcastEnded:
		return db.Model(&Broadcast{}).Where("id = ? AND ended_at IS NULL", event.BroadcastID).
			Updates(map[string]any{
				"ended_at":       event.At,
				"end_reason":     event.Reason,
				"paused_seconds": gorm.Expr("paused_seconds + ?", r.takePaused(event)),
			}).Error
	}
	return fmt.Errorf("unknown usage event kind %q", event.Kind)
}

// takePaused는 진행 중인 일시정지를 event 시각에서 끊고 그 길이(초)를 돌려준다.
func (r *Recorder) takePaused(event session.UsageEvent) float64 {
	start, paused := r.pausedAt[event.BroadcastID]
	if !paused {
		return 0
	}
	delete(r.pausedAt, event.BroadcastID)
	if elapsed := event.At.Sub(start); elapsed > 0 {
		return elapsed.Seconds()
	}
	return 0
}
