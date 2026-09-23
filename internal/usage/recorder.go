package usage

import (
	"context"
	"errors"
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

// defaultRetryDelays는 쓰기 재시도 간격이다. 합계(약 7초)가 Postgres 재시작 같은
// 짧은 중단을 넘기도록 잡았다. 워커 하나가 순서대로 쓰므로 재시도하는 동안 뒤
// 사건은 버퍼에서 기다린다.
var defaultRetryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// errInvalidEvent는 다시 써도 성공할 수 없는 사건이다.
var errInvalidEvent = errors.New("invalid usage event")

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
	// pausedAt은 일시정지 중인 송출의 시작 시각, pausedTotal은 끝난 일시정지의
	// 누적(초)이다. 워커만 만진다. 누적은 송출 종료 때 한 번에 쓴다 — 재개마다
	// 더해 쓰면, 커밋됐지만 응답만 늦은 쓰기를 재시도할 때 두 번 더해진다.
	pausedAt    map[string]time.Time
	pausedTotal map[string]float64
	retryDelays []time.Duration
}

func NewRecorder(db *gorm.DB, logger *slog.Logger) *Recorder {
	return newRecorder(db, logger, defaultRetryDelays)
}

func newRecorder(db *gorm.DB, logger *slog.Logger, retryDelays []time.Duration) *Recorder {
	r := &Recorder{
		db:          db,
		logger:      logger,
		events:      make(chan session.UsageEvent, eventBuffer),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		pausedAt:    make(map[string]time.Time),
		pausedTotal: make(map[string]float64),
		retryDelays: retryDelays,
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
	// 메모리 상태 전이는 재시도와 무관하게 한 번만 한다.
	var pausedSeconds float64
	switch event.Kind {
	case session.UsageBroadcastPaused:
		if _, paused := r.pausedAt[event.BroadcastID]; !paused {
			r.pausedAt[event.BroadcastID] = event.At
		}
		return
	case session.UsageBroadcastResumed:
		r.pausedTotal[event.BroadcastID] += r.takePaused(event)
		return
	case session.UsageBroadcastEnded:
		pausedSeconds = r.pausedTotal[event.BroadcastID] + r.takePaused(event)
		delete(r.pausedTotal, event.BroadcastID)
	}

	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
		rows, err := r.apply(ctx, event, pausedSeconds)
		cancel()
		if err == nil {
			// 재시도에서의 0행은 앞선 시도가 실제로는 커밋된 경우라 경고하지 않는다.
			if rows == 0 && attempt == 0 && isUpdate(event.Kind) {
				r.logger.Warn("usage row not found for update; its start event was lost",
					"kind", event.Kind, "session_id", event.SessionID, "broadcast_id", event.BroadcastID)
			}
			return
		}
		if permanentError(err) || attempt >= len(r.retryDelays) {
			r.logger.Error("record usage event failed; event lost", "kind", event.Kind,
				"session_id", event.SessionID, "broadcast_id", event.BroadcastID, "attempts", attempt+1, "error", err)
			return
		}
		r.logger.Warn("record usage event failed; retrying", "kind", event.Kind,
			"session_id", event.SessionID, "broadcast_id", event.BroadcastID, "attempt", attempt+1, "error", err)
		time.Sleep(r.retryDelays[attempt])
	}
}

func isUpdate(kind session.UsageEventKind) bool {
	return kind == session.UsageSessionEnded || kind == session.UsageBroadcastLive || kind == session.UsageBroadcastEnded
}

// permanentError는 다시 써도 같은 결과인 오류다. 제약 위반은 GORM이
// TranslateError로 번역한 값으로 가른다(internal/database 참고).
func permanentError(err error) bool {
	return errors.Is(err, errInvalidEvent) ||
		errors.Is(err, gorm.ErrForeignKeyViolated) ||
		errors.Is(err, gorm.ErrDuplicatedKey) ||
		errors.Is(err, gorm.ErrCheckConstraintViolated)
}

// apply는 사건 하나를 쓰고 영향받은 행 수를 돌려준다. 재시도해도 결과가 같아야
// 한다 — 삽입은 ON CONFLICT DO NOTHING, 갱신은 아직 비어 있는 칸만 채운다.
func (r *Recorder) apply(ctx context.Context, event session.UsageEvent, pausedSeconds float64) (int64, error) {
	db := r.db.WithContext(ctx)
	var result *gorm.DB
	switch event.Kind {
	case session.UsageSessionStarted:
		sessionID, err := uuid.Parse(event.SessionID)
		if err != nil {
			return 0, fmt.Errorf("%w: session id: %v", errInvalidEvent, err)
		}
		row := Session{ID: sessionID, IsGuest: event.UserID == uuid.Nil, StartedAt: event.At, Source: SourceLive}
		if event.UserID != uuid.Nil {
			row.UserID = &event.UserID
		}
		if event.AIProcessing != "" {
			row.AIProcessing = &event.AIProcessing
		}
		result = db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)

	case session.UsageSessionEnded:
		result = db.Model(&Session{}).Where("session_id = ? AND ended_at IS NULL", event.SessionID).
			Updates(map[string]any{"ended_at": event.At, "end_reason": event.Reason})

	case session.UsageBroadcastStarted:
		id, err := uuid.Parse(event.BroadcastID)
		if err != nil {
			return 0, fmt.Errorf("%w: broadcast id: %v", errInvalidEvent, err)
		}
		sessionID, err := uuid.Parse(event.SessionID)
		if err != nil {
			return 0, fmt.Errorf("%w: session id: %v", errInvalidEvent, err)
		}
		row := Broadcast{ID: id, SessionID: sessionID, Provider: event.Provider, StartedAt: event.At, Source: SourceLive}
		result = db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)

	case session.UsageBroadcastLive:
		result = db.Model(&Broadcast{}).Where("id = ? AND live_at IS NULL", event.BroadcastID).
			Update("live_at", event.At)

	case session.UsageBroadcastEnded:
		result = db.Model(&Broadcast{}).Where("id = ? AND ended_at IS NULL", event.BroadcastID).
			Updates(map[string]any{"ended_at": event.At, "end_reason": event.Reason, "paused_seconds": pausedSeconds})

	default:
		return 0, fmt.Errorf("%w: unknown kind %q", errInvalidEvent, event.Kind)
	}
	return result.RowsAffected, result.Error
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
