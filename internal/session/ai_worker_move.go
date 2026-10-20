package session

import (
	"time"
)

// aiMoveCooldown은 한 세션의 파이프라인을 AI 워커 이동으로 다시 띄우는 최소
// 간격이다. 워커 상태가 오락가락해도 세션이 재시작을 되풀이하지 않게 한다.
const aiMoveCooldown = 30 * time.Second

// handleAIReadinessChange는 AI 풀의 준비 상태가 바뀔 때 불린다(#499). 바뀔 때마다
// 전체를 다시 본다 — 워커 하나가 돌아오면, 갈 곳이 없어 다른 죽은 워커에 남아
// 있던 세션도 이제 옮길 수 있다.
func (m *Manager) handleAIReadinessChange(string, bool) {
	go m.moveSessionsOffUnreadyAIWorkers()
}

// moveSessionsOffUnreadyAIWorkers는 응답 불가 워커에 배정된 세션의 영상
// 파이프라인을 다시 띄운다. 다시 띄운 파이프라인은 준비된 워커를 배정받는다.
//
// 응답 불가 워커는 프레임마다 실패해 세션을 fail-closed 블러로 잠그고, 잠긴 세션의
// 회복 시도도 같은 워커로만 간다. 워커가 재시작될 때까지 세션이 끝까지 블러였던
// 2026-10-20 장애가 이 경로다. 옮길 곳이 없으면 그대로 두고, 워커가 돌아오는
// 상태 변화 때 다시 본다.
func (m *Manager) moveSessionsOffUnreadyAIWorkers() {
	moves, retryIn := m.aiWorkerMoves(time.Now())
	for _, move := range moves {
		go m.moveVideoPipelineOffAIWorker(move.session, move.from)
	}
	if retryIn > 0 {
		// 방금 옮긴 세션이 옮겨 간 워커도 죽었다. 대기가 끝나면 다시 본다 — 그
		// 사이 상태 변화가 없으면 아무도 이 세션을 다시 보지 않는다.
		time.AfterFunc(retryIn, m.moveSessionsOffUnreadyAIWorkers)
	}
}

type aiWorkerMove struct {
	session *Session
	from    string
}

// aiWorkerMoves는 지금 옮길 세션과, 이동 대기(aiMoveCooldown) 중이라 미룬 세션을
// 다시 볼 때까지의 시간을 돌려준다. 응답 불가 워커에 배정됐고 갈 곳(다른 준비된
// 워커)이 있는 세션만 고른다.
func (m *Manager) aiWorkerMoves(now time.Time) ([]aiWorkerMove, time.Duration) {
	if m.ai == nil {
		return nil, 0
	}
	m.mu.RLock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.RUnlock()

	var moves []aiWorkerMove
	var retryIn time.Duration
	for _, s := range sessions {
		s.mu.RLock()
		from := s.aiTarget
		wait := aiMoveCooldown - now.Sub(s.aiMovedAt)
		s.mu.RUnlock()
		if from == "" || m.ai.TargetReady(from) || !m.ai.HasReadyTargetOtherThan(from) {
			continue
		}
		if wait > 0 {
			if retryIn == 0 || wait < retryIn {
				retryIn = wait
			}
			continue
		}
		moves = append(moves, aiWorkerMove{session: s, from: from})
	}
	return moves, retryIn
}

// moveVideoPipelineOffAIWorker는 from 워커에 배정된 세션의 영상 파이프라인을 같은
// 트랙으로 다시 띄운다. 해상도 변경(#283)과 같은 재시작 경로다. 디코더 핀이 그대로라
// 송출 중에도 다시 띄울 수 있고, egress는 세션 슬롯을 통하므로 끊기지 않는다(#83).
// 재시작 사이에는 egress가 마지막 프레임(잠긴 동안의 블러 프레임)을 채운다.
func (m *Manager) moveVideoPipelineOffAIWorker(s *Session, from string) {
	s.resolutionMu.Lock()
	defer s.resolutionMu.Unlock()

	s.mu.Lock()
	track, done := s.videoTrack, s.pipelineDone
	// 기다리는 사이 세션이 닫혔거나, 파이프라인이 이미 다른 워커로 갔거나, 다른
	// 경로가 다시 띄우는 중이면 할 일이 없다.
	restart := !s.closed && s.aiTarget == from && time.Since(s.aiMovedAt) >= aiMoveCooldown &&
		track != nil && s.rawTrackID == track.ID() && s.trackCancel != nil
	if !restart {
		s.mu.Unlock()
		return
	}
	s.aiMovedAt = time.Now()
	s.trackCancel()
	s.trackCancel = nil
	s.mu.Unlock()

	m.logger.Warn("moving video pipeline off unavailable AI worker", "session_id", s.ID, "from_target", from)
	m.relaunchVideoPipeline(s, track, done)
}
