package server

import (
	"context"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/media"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"
)

const (
	// chzzkIngestStableFor는 송출이 끊기지 않고 이어져야 받아들여진 것으로 보는
	// 시간이다. 다른 도구가 같은 스트림 키로 방송 중이면 치지직은 3~6초마다
	// 연결을 끊는다(2026-09-29 실측, #361).
	chzzkIngestStableFor = 8 * time.Second
	// chzzkIngestMaxWatch는 판정을 기다리는 최대 시간이다. 순간적인 망 끊김은
	// 재연결 뒤 다시 8초를 채우면 통과한다 — 사용자에게 티 나지 않게. 거절은
	// 재연결 예산(10초)이 다하면 송출이 멈추는 것으로 확정된다.
	chzzkIngestMaxWatch     = 40 * time.Second
	chzzkIngestPollInterval = 500 * time.Millisecond
	// noticeChannelLiveElsewhere는 채널이 다른 도구로 방송 중이라 송출이 거절됐다는
	// 세션 알림이다.
	noticeChannelLiveElsewhere = "channel_live_elsewhere"
)

// applyChzzkSettingsWhenAccepted는 치지직이 송출을 받아들인 것을 확인한 뒤에 방송
// 설정(제목·카테고리·태그)을 적용한다(#361). 치지직 설정은 채널 전역값이라, 다른
// 도구로 방송 중인 채널에 먼저 적용하면 그 방송의 제목이 바뀐다. 공식 API에는
// 채널의 라이브 여부를 묻는 방법이 없어 송출이 거절되는지로 판별한다. 그 대가로
// 정상 방송도 첫 몇 초는 채널의 이전 설정으로 보인다.
func (s *Server) applyChzzkSettingsWhenAccepted(liveSession *session.Session, settings session.ChzzkBroadcastSettings) {
	updater, ok := s.streaming[auth.StreamingProviderChzzk].(streaming.LiveUpdater)
	if !ok {
		return
	}
	provider := string(auth.StreamingProviderChzzk)
	watch := newIngestWatch(time.Now())
	for {
		now := time.Now()
		verdict := watch.observe(liveSession.TargetStream(provider), now)
		if verdict == ingestAccepted {
			break
		}
		if verdict == ingestRejected {
			if liveSession.AddNotice(noticeChannelLiveElsewhere, now) {
				s.logger.Warn("chzzk ingest rejected; channel may be live from another tool", "session_id", liveSession.ID)
			}
			return
		}
		if verdict == ingestEnded {
			return // 그 사이 방송이 끝났다.
		}
		if now.Sub(watch.started) > chzzkIngestMaxWatch {
			s.logger.Warn("chzzk ingest never stabilized; settings not applied", "session_id", liveSession.ID)
			return
		}
		time.Sleep(chzzkIngestPollInterval)
	}
	update := chzzkLiveUpdateFrom(settings)
	ctx, cancel := context.WithTimeout(context.Background(), platformCleanupTimeout)
	defer cancel()
	if err := updater.UpdateLive(ctx, liveSession.UserID, streaming.PreparedBroadcast{Provider: auth.StreamingProviderChzzk}, update); err != nil {
		s.logger.Error("apply chzzk settings after ingest failed", "session_id", liveSession.ID, "error", err)
		return
	}
	s.logger.Info("chzzk settings applied after ingest accepted", "session_id", liveSession.ID)
}

type ingestVerdict int

const (
	ingestPending ingestVerdict = iota
	ingestAccepted
	ingestRejected
	ingestEnded
)

// ingestWatch는 치지직 송출이 받아들여졌는지 지켜본다. 재연결이 일어날 때마다
// 안정 시계를 다시 시작하고, 송출이 멈추면(재연결 예산 소진) 거절로 확정한다.
type ingestWatch struct {
	started      time.Time
	stableSince  time.Time
	lastAttempts int
}

func newIngestWatch(now time.Time) *ingestWatch {
	return &ingestWatch{started: now, stableSince: now}
}

func (w *ingestWatch) observe(stream session.StreamState, now time.Time) ingestVerdict {
	switch {
	case stream.Status == string(media.EgressPhaseStopped):
		return ingestRejected
	case stream.BroadcastPhase != session.BroadcastPhaseLive:
		return ingestEnded
	}
	if stream.ReconnectAttempts != w.lastAttempts || stream.Status != string(media.EgressPhaseStreaming) {
		w.lastAttempts = stream.ReconnectAttempts
		w.stableSince = now
		return ingestPending
	}
	if now.Sub(w.stableSince) >= chzzkIngestStableFor {
		return ingestAccepted
	}
	return ingestPending
}

// chzzkLiveUpdateFrom은 저장한 치지직 설정을 방송 중 설정 변경으로 옮긴다. 빈
// 카테고리는 저장한 설정일 때만 지우기로 보낸다(#352) — 저장한 적이 없으면 채널
// 카테고리를 건드리지 않는다.
func chzzkLiveUpdateFrom(settings session.ChzzkBroadcastSettings) streaming.LiveUpdate {
	update := streaming.LiveUpdate{Title: &settings.Title, Tags: settings.Tags, TagsSet: settings.Tags != nil}
	if settings.CategoryID != "" || !settings.UpdatedAt.IsZero() {
		update.CategoryType, update.CategoryID = &settings.CategoryType, &settings.CategoryID
	}
	return update
}
