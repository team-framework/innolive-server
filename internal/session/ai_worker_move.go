package session

import (
	"context"
	"errors"
	"sync"
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

// moveVideoPipelineOffAIWorker는 from 워커에 배정된 세션을 준비된 다른 워커로 옮긴다.
// 먼저 파이프라인을 둔 채 AI 스트림만 넘긴다(#515). 디코더·인코더를 다시 띄우지
// 않으므로 송출이 끊기지 않고, 블러로 잠긴 세션도 회복 시도 프레임으로 새 워커를
// 예열한다. 넘기지 못하면 같은 트랙으로 파이프라인을 다시 띄운다. 해상도 변경(#283)과
// 같은 재시작 경로다. 디코더 핀이 그대로라 송출 중에도 다시 띄울 수 있고, egress는
// 세션 슬롯을 통하므로 끊기지 않는다(#83). 재시작 사이에는 egress가 마지막 프레임(잠긴
// 동안의 블러 프레임)을 채운다.
func (m *Manager) moveVideoPipelineOffAIWorker(s *Session, from string) {
	s.resolutionMu.Lock()
	defer s.resolutionMu.Unlock()

	s.mu.Lock()
	// 기다리는 사이 세션이 닫혔거나, 파이프라인이 이미 다른 워커로 갔거나, 방금 옮겼으면
	// 할 일이 없다.
	if s.closed || s.aiTarget != from || time.Since(s.aiMovedAt) < aiMoveCooldown {
		s.mu.Unlock()
		return
	}
	s.aiMovedAt = time.Now()
	s.mu.Unlock()

	moved, err := m.switchSessionAIWorker(context.Background(), s, from)
	if moved {
		return
	}
	if err != nil {
		m.logger.Warn("switching AI stream off unavailable worker failed; restarting video pipeline",
			"session_id", s.ID, "from_target", from, "error", err)
	}

	s.mu.Lock()
	track, done := s.videoTrack, s.pipelineDone
	// 다른 경로가 다시 띄우는 중이면 할 일이 없다.
	restart := !s.closed && s.aiTarget == from && track != nil && s.rawTrackID == track.ID() && s.trackCancel != nil
	if !restart {
		s.mu.Unlock()
		return
	}
	s.trackCancel()
	s.trackCancel = nil
	s.mu.Unlock()

	m.logger.Warn("moving video pipeline off unavailable AI worker", "session_id", s.ID, "from_target", from)
	m.relaunchVideoPipeline(s, track, done)
}

// aiSwitchTimeout은 세션 하나를 다른 워커로 넘기는 한계다. 흐르는 세션은 예열 프레임
// 몇 개면 끝나지만, 블러로 잠긴 세션은 회복 시도 간격(1초)마다 한 프레임만 AI에
// 보내므로 넉넉히 둔다.
const aiSwitchTimeout = 20 * time.Second

// aiSwitchConcurrency는 워커 하나를 비울 때 동시에 넘기는 세션 수다. 예열 프레임이
// 새 워커에 한꺼번에 몰리지 않게 한다.
const aiSwitchConcurrency = 2

var errNoAIMoveTarget = errors.New("no ready AI worker to move the session to")

// switchSessionAIWorker는 from 워커에 있는 세션의 AI 스트림을 준비된 다른 워커로
// 넘긴다(#515). 세션이 이미 from에 없거나 AI 스트림이 없으면 moved=false, err=nil이다.
func (m *Manager) switchSessionAIWorker(ctx context.Context, s *Session, from string) (moved bool, err error) {
	s.mu.RLock()
	stream, target := s.aiStream, s.aiTarget
	faceSwap := s.FaceAnonymization == FaceAnonymizationFaceSwap
	s.mu.RUnlock()
	if stream == nil || target != from {
		return false, nil
	}
	client, release, ok := m.ai.AcquireMoveTarget(faceSwap, from)
	if !ok {
		return false, errNoAIMoveTarget
	}
	switchCtx, cancel := context.WithTimeout(ctx, aiSwitchTimeout)
	defer cancel()
	if err := stream.SwitchTo(switchCtx, client, release); err != nil {
		return false, err
	}
	to := client.Address()
	s.mu.Lock()
	if s.aiStream == stream {
		s.aiTarget = to
		s.aiMovedAt = time.Now()
	}
	s.mu.Unlock()
	m.metrics.IncAITargetSession(to)
	m.logger.Info("switched AI stream to another worker", "session_id", s.ID, "from_target", from, "to_target", to)
	return true, nil
}

// AIWorkerSwitchReport는 워커 하나를 비운 결과다.
type AIWorkerSwitchReport struct {
	Moved int `json:"moved"`
	// Failed는 넘기지 못한 세션 id와 사유다. 그 세션은 원래 워커에 그대로 있다.
	Failed map[string]string `json:"failed,omitempty"`
}

// SwitchSessionsOffAIWorker는 from 워커에 배정된 모든 세션의 AI 스트림을 다음
// 프레임부터 다른 워커로 넘긴다(#515). 워커 교체 배포가 옛 워커를 비울 때 쓴다.
// 호출 전에 from을 배정 제외로 둬야 그 사이 새 세션이 들어오지 않는다.
func (m *Manager) SwitchSessionsOffAIWorker(ctx context.Context, from string) AIWorkerSwitchReport {
	report := AIWorkerSwitchReport{}
	if m.ai == nil {
		return report
	}
	m.mu.RLock()
	var sessions []*Session
	for _, s := range m.sessions {
		s.mu.RLock()
		if s.aiTarget == from && s.aiStream != nil {
			sessions = append(sessions, s)
		}
		s.mu.RUnlock()
	}
	m.mu.RUnlock()

	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, aiSwitchConcurrency)
	for _, s := range sessions {
		wg.Add(1)
		slots <- struct{}{}
		go func(s *Session) {
			defer wg.Done()
			defer func() { <-slots }()
			moved, err := m.switchSessionAIWorker(ctx, s, from)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case moved:
				report.Moved++
			case err != nil:
				if report.Failed == nil {
					report.Failed = make(map[string]string)
				}
				report.Failed[s.ID] = err.Error()
			}
		}(s)
	}
	wg.Wait()
	return report
}
