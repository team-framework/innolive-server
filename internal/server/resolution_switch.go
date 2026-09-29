package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"
)

// 방송 중 송출 방식 전환(#283 해상도, #300 대상 구성). 유튜브·치지직 모두 같은
// 방송 안의 해상도 변경을 반영하지 않으므로(2026-09-28 실측: 재연결로 1080p를 보내도
// 화질 단계가 그대로) 해상도가 바뀌면 서버가 대상을 끝내고 새 해상도로 다시 준비·
// 라이브 전환한다. 새 방송이라 유튜브는 시청 URL이 바뀐다.
var (
	// resolutionSwitchChzzkWait는 치지직 대상을 끝낸 뒤 다시 붙기까지 기다리는
	// 시간이다. 치지직은 RTMP가 끊긴 뒤 13~14초 안에 다시 붙으면 같은 방송으로
	// 이어 붙인다(chzzkEgressReconnectMaxElapsed 참고) — 그러면 해상도가 그대로다.
	resolutionSwitchChzzkWait = 15 * time.Second
	// resolutionSwitchGoLiveTimeout은 새 방송의 라이브 전환을 재시도하는 한도다.
	// 유튜브는 송출이 플랫폼에 도착하기 전까지 전환을 거절한다(broadcast_not_ready).
	resolutionSwitchGoLiveTimeout = 60 * time.Second
	resolutionSwitchRetryInterval = 2 * time.Second
	// resolutionSwitchPauseTimeout은 새 방송을 다시 멈추기까지 기다리는 한도다.
	// egress는 첫 프레임으로 출력 형식을 정한 뒤에야 슬레이트로 바꿀 수 있다.
	resolutionSwitchPauseTimeout  = 15 * time.Second
	resolutionSwitchPauseInterval = 100 * time.Millisecond
	// resolutionSwitchTimeout은 전환 작업 전체의 상한이다.
	resolutionSwitchTimeout = 3 * time.Minute
)

// handlePutBroadcastResolution은 세션의 송출 해상도를 바꾼다(#283). 방송 중이 아니면
// 바로 바꾸고(200), 라이브 대상이 있으면 대상 구성은 그대로 둔 송출 방식 전환을
// 시작하고 202를 돌려준다. 진행은 세션 응답의 resolution_switch로 본다.
func (s *Server) handlePutBroadcastResolution(w http.ResponseWriter, r *http.Request, liveSession *session.Session) {
	request := struct {
		Resolution string `json:"resolution"`
	}{}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := decodeOptionalJSON(r.Body, &request); err != nil {
		writeError(w, badRequest("Invalid resolution request.", map[string]any{"error": err.Error()}))
		return
	}
	resolution := strings.TrimSpace(request.Resolution)
	if resolution != session.Resolution720p && resolution != session.ResolutionFHD {
		writeError(w, badRequest("resolution must be 720p or fhd.", map[string]any{"resolution": request.Resolution}))
		return
	}
	if liveSession.ResolutionSwitching() {
		writeError(w, switchInProgressError(liveSession.ID))
		return
	}
	liveTargets, _ := liveSession.BroadcastActivity()
	if len(liveTargets) > 0 {
		s.startBroadcastSwitch(w, liveSession, resolution, liveTargets)
		return
	}
	if liveSession.Resolution() == resolution {
		writeJSON(w, http.StatusOK, liveSession.Response())
		return
	}
	if liveSession.BusyTargetCount() > 0 {
		writeError(w, broadcastBusyError(liveSession.ID))
		return
	}
	if gate := planGateError(liveSession.Plan, resolution == session.ResolutionFHD, 1); gate != nil {
		writeError(w, *gate)
		return
	}
	if _, err := s.sessions.ChangeBroadcastResolution(liveSession.ID, resolution); err != nil {
		if errors.Is(err, session.ErrResolutionChangeWhileLive) {
			writeError(w, broadcastBusyError(liveSession.ID))
			return
		}
		writeError(w, *startStreamError(err, liveSession.ID))
		return
	}
	writeJSON(w, http.StatusOK, liveSession.Response())
}

// handlePutBroadcastMode는 방송 중 송출 방식(해상도·대상 구성)을 바꾼다(#300).
// 해상도를 생략하면 지금 해상도다. 방송 중이 아니면 대상 구성은 방송 준비가 정하므로
// 409다.
func (s *Server) handlePutBroadcastMode(w http.ResponseWriter, r *http.Request, liveSession *session.Session) {
	request := struct {
		Resolution string   `json:"resolution"`
		Targets    []string `json:"targets"`
	}{}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := decodeOptionalJSON(r.Body, &request); err != nil {
		writeError(w, badRequest("Invalid broadcast mode request.", map[string]any{"error": err.Error()}))
		return
	}
	resolution := strings.TrimSpace(request.Resolution)
	if resolution == "" {
		resolution = liveSession.Resolution()
	}
	if resolution != session.Resolution720p && resolution != session.ResolutionFHD {
		writeError(w, badRequest("resolution must be 720p or fhd.", map[string]any{"resolution": request.Resolution}))
		return
	}
	var targets []string
	for _, raw := range request.Targets {
		provider := auth.StreamingProvider(strings.TrimSpace(raw))
		if !provider.Valid() || s.streaming[provider] == nil {
			writeError(w, badRequest("Unknown or unavailable streaming provider.", map[string]any{"provider": raw}))
			return
		}
		if !slices.Contains(targets, string(provider)) {
			targets = append(targets, string(provider))
		}
	}
	if len(targets) == 0 {
		writeError(w, badRequest("targets must name at least one provider.", nil))
		return
	}
	if liveSession.ResolutionSwitching() {
		writeError(w, switchInProgressError(liveSession.ID))
		return
	}
	liveTargets, _ := liveSession.BroadcastActivity()
	if len(liveTargets) == 0 {
		writeError(w, apiError{Status: http.StatusConflict, Code: "broadcast_not_live", Message: "Change the broadcast mode while live. Before going live, prepare the targets you want.", Details: map[string]any{"session_id": liveSession.ID}})
		return
	}
	s.startBroadcastSwitch(w, liveSession, resolution, targets)
}

func switchInProgressError(sessionID string) apiError {
	return apiError{Status: http.StatusConflict, Code: "resolution_switch_in_progress", Message: "The broadcast mode is already being switched.", Details: map[string]any{"session_id": sessionID}}
}

func broadcastBusyError(sessionID string) apiError {
	return apiError{Status: http.StatusConflict, Code: "broadcast_busy", Message: "A broadcast is being prepared. Change the broadcast mode once it is live or stopped.", Details: map[string]any{"session_id": sessionID}}
}

// startBroadcastSwitch는 방송 중 송출 방식 전환을 검증하고 시작한다. 플랜과 자리는
// 전환 뒤 구성으로 판정하며, 방송을 건드리기 전에 거절한다.
func (s *Server) startBroadcastSwitch(w http.ResponseWriter, liveSession *session.Session, resolution string, targets []string) {
	liveTargets, _ := liveSession.BroadcastActivity()
	slices.Sort(liveTargets)
	targets = slices.Clone(targets)
	slices.Sort(targets)
	// 준비·라이브 전환 중인 대상은 끝낼 수도 이어 갈 수도 없는 중간 상태다.
	if liveSession.BusyTargetCount() > len(liveTargets) {
		writeError(w, broadcastBusyError(liveSession.ID))
		return
	}
	if resolution == liveSession.Resolution() && slices.Equal(liveTargets, targets) {
		writeJSON(w, http.StatusOK, liveSession.Response())
		return
	}
	if gate := planGateError(liveSession.Plan, resolution == session.ResolutionFHD, len(targets)); gate != nil {
		writeError(w, *gate)
		return
	}
	if err := s.sessions.CheckBroadcastUnits(liveSession.ID, resolution, len(targets)); err != nil {
		writeError(w, *startStreamError(err, liveSession.ID))
		return
	}
	if !liveSession.BeginResolutionSwitch(resolution, targets) {
		writeError(w, switchInProgressError(liveSession.ID))
		return
	}
	go s.runBroadcastSwitch(liveSession, resolution, liveTargets, targets, pausedAfterSwitch(liveTargets, liveSession.PausedTargets(), targets))
	writeJSON(w, http.StatusAccepted, liveSession.Response())
}

// pausedAfterSwitch는 전환 뒤 멈춘 채로 열 대상이다. 라이브 대상이 전부 멈춰
// 있었다면 방송 전체를 멈춘 상태이므로 새로 추가한 대상까지 멈춘다 — 자리를 비운
// 스트리머의 화면이 새 플랫폼으로 나가면 안 된다(#320). 일부만 멈췄다면 멈춰 있던
// 대상만 다시 멈춘다.
func pausedAfterSwitch(live, paused, to []string) []string {
	if len(live) > 0 && len(paused) >= len(live) {
		return slices.Clone(to)
	}
	return paused
}

// runBroadcastSwitch는 라이브 구성(from)을 새 구성(to)으로 바꾼다.
//   - 해상도가 같으면 계속되는 대상은 건드리지 않는다. 빠지는 대상만 끝내고 추가되는
//     대상만 연다.
//   - 해상도가 바뀌면 플랫폼이 같은 방송 안의 변경을 반영하지 않으므로 모든 대상을
//     끝내고, 해상도를 바꾼 뒤, 새 구성의 대상을 새 방송으로 연다.
//
// 한 대상이 실패해도 나머지는 계속하고 되돌리지 않는다(동시 발사와 같은 규칙).
// 유튜브를 먼저 열고, 방금 끝낸 치지직은 이전 방송이 닫힐 때까지 기다렸다가 연다.
// 멈춰 있던 대상(paused)은 새 방송을 연 뒤 다시 멈춘다.
func (s *Server) runBroadcastSwitch(liveSession *session.Session, resolution string, from, to, paused []string) {
	ctx, cancel := context.WithTimeout(context.Background(), resolutionSwitchTimeout)
	defer cancel()
	started := time.Now()
	changeResolution := resolution != liveSession.Resolution()
	stop, open := from, to
	stopReason := "resolution_change"
	if !changeResolution {
		stop = slices.DeleteFunc(slices.Clone(from), func(p string) bool { return slices.Contains(to, p) })
		open = slices.DeleteFunc(slices.Clone(to), func(p string) bool { return slices.Contains(from, p) })
		stopReason = "target_removed"
	}
	s.logger.Info("broadcast switch started", "session_id", liveSession.ID, "to", resolution,
		"from_targets", from, "to_targets", to, "paused", paused)
	var failures []session.ResolutionSwitchFailure
	fail := func(provider, code string) {
		failures = append(failures, session.ResolutionSwitchFailure{Provider: provider, Code: code})
	}

	for _, provider := range stop {
		_, broadcast, phase, err := s.sessions.StopStreamWithReason(liveSession.ID, stopReason, provider)
		if err != nil {
			s.logger.Warn("broadcast switch stop failed", "session_id", liveSession.ID, "provider", provider, "error", err)
			continue
		}
		s.disposeBroadcast(liveSession.UserID, broadcast, phase)
	}
	stoppedAt := time.Now()
	for _, provider := range stop {
		if err := s.sessions.WaitStreamEnded(ctx, liveSession.ID, provider); err != nil {
			s.logger.Warn("broadcast switch wait for stop failed", "session_id", liveSession.ID, "provider", provider, "error", err)
		}
	}
	if changeResolution {
		if _, err := s.sessions.ChangeBroadcastResolution(liveSession.ID, resolution); err != nil {
			s.logger.Error("broadcast switch resolution change failed", "session_id", liveSession.ID, "error", err)
			for _, provider := range open {
				fail(provider, "resolution_change_failed")
			}
			liveSession.FinishResolutionSwitch(failures)
			return
		}
	}

	var delayed []string
	for _, provider := range open {
		if auth.StreamingProvider(provider) == auth.StreamingProviderChzzk && slices.Contains(stop, provider) {
			delayed = append(delayed, provider)
			continue
		}
		if failure := s.reopenTarget(ctx, liveSession, provider, slices.Contains(paused, provider)); failure != nil {
			fail(provider, failure.Code)
		}
	}
	if len(delayed) > 0 && s.waitResolutionSwitch(ctx, liveSession, time.Until(stoppedAt.Add(resolutionSwitchChzzkWait))) {
		for _, provider := range delayed {
			if failure := s.reopenTarget(ctx, liveSession, provider, slices.Contains(paused, provider)); failure != nil {
				fail(provider, failure.Code)
			}
		}
	} else {
		for _, provider := range delayed {
			fail(provider, "resolution_switch_canceled")
		}
	}
	liveSession.FinishResolutionSwitch(failures)
	s.logger.Info("broadcast switch finished", "session_id", liveSession.ID, "to", resolution,
		"failed_targets", len(failures), "elapsed_ms", time.Since(started).Milliseconds())
}

// reopenTarget은 대상 하나의 새 방송을 준비하고 라이브로 전환한다. 전환에 실패하면
// 준비한 방송을 치운다 — 준비 상태로 남기면 사용자가 모르는 방송이 채널에 남는다.
// pause면 라이브가 된 뒤 다시 멈춘다.
func (s *Server) reopenTarget(ctx context.Context, liveSession *session.Session, provider string, pause bool) *apiError {
	canceled := &apiError{Status: http.StatusConflict, Code: "resolution_switch_canceled", Message: "The resolution switch was canceled."}
	if liveSession.ResolutionSwitchCanceled() {
		return canceled
	}
	providerName := auth.StreamingProvider(provider)
	if _, failure := s.prepareTarget(ctx, liveSession, providerName); failure != nil {
		return failure
	}
	deadline := time.Now().Add(resolutionSwitchGoLiveTimeout)
	for {
		failure := s.goLiveTarget(ctx, liveSession, providerName)
		if failure == nil {
			if pause {
				s.pauseReopenedTarget(ctx, liveSession, provider)
			}
			return nil
		}
		retry := failure.Code == "broadcast_not_ready" && time.Now().Before(deadline)
		if !retry || !s.waitResolutionSwitch(ctx, liveSession, resolutionSwitchRetryInterval) {
			if retry {
				failure = canceled
			}
			if _, broadcast, phase, err := s.sessions.StopStreamWithReason(liveSession.ID, "resolution_change", provider); err == nil {
				s.disposeBroadcast(liveSession.UserID, broadcast, phase)
			}
			return failure
		}
	}
}

// pauseReopenedTarget은 새 방송을 다시 멈춘다. egress가 첫 프레임으로 출력 형식을
// 정하기 전에는 멈출 수 없으므로(stream_not_active) 짧게 재시도한다. 끝내 못 멈추면
// 경고만 남긴다 — 방송은 열린 채로 둔다.
func (s *Server) pauseReopenedTarget(ctx context.Context, liveSession *session.Session, provider string) {
	deadline := time.Now().Add(resolutionSwitchPauseTimeout)
	for {
		_, err := s.sessions.PauseStream(liveSession.ID, provider)
		if err == nil || errors.Is(err, session.ErrStreamPaused) {
			return
		}
		if !errors.Is(err, session.ErrStreamNotActive) || time.Now().After(deadline) || !s.waitResolutionSwitch(ctx, liveSession, resolutionSwitchPauseInterval) {
			s.logger.Warn("resolution switch could not pause the reopened broadcast", "session_id", liveSession.ID, "provider", provider, "error", err)
			return
		}
	}
}

// waitResolutionSwitch는 d만큼 기다린다. 그 사이 작업 시간이 다했거나, 세션이
// 닫혔거나, 전환이 취소됐으면 false다.
func (s *Server) waitResolutionSwitch(ctx context.Context, liveSession *session.Session, d time.Duration) bool {
	if d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-liveSession.Done():
			return false
		case <-timer.C:
		}
	}
	return !liveSession.ResolutionSwitchCanceled()
}
