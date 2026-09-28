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

// 방송 중 해상도 전환(#283). 유튜브·치지직 모두 같은 방송 안의 해상도 변경을
// 반영하지 않으므로(2026-09-28 실측: 재연결로 1080p를 보내도 화질 단계가 그대로)
// 서버가 대상을 끝내고 새 해상도로 다시 준비·라이브 전환한다. 새 방송이라 유튜브는
// 시청 URL이 바뀐다.
var (
	// resolutionSwitchChzzkWait는 치지직 대상을 끝낸 뒤 다시 붙기까지 기다리는
	// 시간이다. 치지직은 RTMP가 끊긴 뒤 13~14초 안에 다시 붙으면 같은 방송으로
	// 이어 붙인다(chzzkEgressReconnectMaxElapsed 참고) — 그러면 해상도가 그대로다.
	resolutionSwitchChzzkWait = 15 * time.Second
	// resolutionSwitchGoLiveTimeout은 새 방송의 라이브 전환을 재시도하는 한도다.
	// 유튜브는 송출이 플랫폼에 도착하기 전까지 전환을 거절한다(broadcast_not_ready).
	resolutionSwitchGoLiveTimeout = 60 * time.Second
	resolutionSwitchRetryInterval = 2 * time.Second
	// resolutionSwitchTimeout은 전환 작업 전체의 상한이다.
	resolutionSwitchTimeout = 3 * time.Minute
)

// handlePutBroadcastResolution은 세션의 송출 해상도를 바꾼다(#283). 방송 중이 아니면
// 바로 바꾸고(200), 라이브 대상이 있으면 새 방송으로의 전환을 시작하고 202를
// 돌려준다. 진행은 세션 응답의 resolution_switch로 본다.
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
		writeError(w, apiError{Status: http.StatusConflict, Code: "resolution_switch_in_progress", Message: "The resolution is already being switched.", Details: map[string]any{"session_id": liveSession.ID}})
		return
	}
	if liveSession.Resolution() == resolution {
		writeJSON(w, http.StatusOK, liveSession.Response())
		return
	}
	liveTargets, _ := liveSession.BroadcastActivity()
	slices.Sort(liveTargets)
	// 준비·라이브 전환 중인 대상은 끝낼 수도 이어 갈 수도 없는 중간 상태다.
	if liveSession.BusyTargetCount() > len(liveTargets) {
		writeError(w, broadcastBusyError(liveSession.ID))
		return
	}
	if gate := planGateError(liveSession.Plan, resolution == session.ResolutionFHD, max(len(liveTargets), 1)); gate != nil {
		writeError(w, *gate)
		return
	}
	if len(liveTargets) == 0 {
		if _, err := s.sessions.ChangeBroadcastResolution(liveSession.ID, resolution); err != nil {
			if errors.Is(err, session.ErrResolutionChangeWhileLive) {
				writeError(w, broadcastBusyError(liveSession.ID))
				return
			}
			writeError(w, *startStreamError(err, liveSession.ID))
			return
		}
		writeJSON(w, http.StatusOK, liveSession.Response())
		return
	}
	if !liveSession.BeginResolutionSwitch(resolution, liveTargets) {
		writeError(w, apiError{Status: http.StatusConflict, Code: "resolution_switch_in_progress", Message: "The resolution is already being switched.", Details: map[string]any{"session_id": liveSession.ID}})
		return
	}
	// 자리는 방송을 끊기 전에 확인한다. 끊고 나서 모자라면 방송만 잃는다.
	if err := s.sessions.ReserveResolutionUnits(liveSession.ID, resolution); err != nil {
		liveSession.DiscardResolutionSwitch()
		writeError(w, *startStreamError(err, liveSession.ID))
		return
	}
	go s.runResolutionSwitch(liveSession, resolution, liveTargets)
	writeJSON(w, http.StatusAccepted, liveSession.Response())
}

func broadcastBusyError(sessionID string) apiError {
	return apiError{Status: http.StatusConflict, Code: "broadcast_busy", Message: "A broadcast is being prepared. Change the resolution once it is live or stopped.", Details: map[string]any{"session_id": sessionID}}
}

// runResolutionSwitch는 라이브 대상을 끝내고, 해상도를 바꾼 뒤, 대상마다 새 방송을
// 준비해 라이브로 전환한다. 한 대상이 실패해도 나머지는 계속한다(동시 발사와 같은
// 규칙). 유튜브를 먼저 열고, 치지직은 이전 방송이 닫힐 때까지 기다렸다가 연다.
func (s *Server) runResolutionSwitch(liveSession *session.Session, resolution string, providers []string) {
	ctx, cancel := context.WithTimeout(context.Background(), resolutionSwitchTimeout)
	defer cancel()
	started := time.Now()
	s.logger.Info("resolution switch started", "session_id", liveSession.ID, "to", resolution, "targets", providers)
	var failures []session.ResolutionSwitchFailure
	fail := func(provider, code string) {
		failures = append(failures, session.ResolutionSwitchFailure{Provider: provider, Code: code})
	}

	for _, provider := range providers {
		_, broadcast, phase, err := s.sessions.StopStreamWithReason(liveSession.ID, "resolution_change", provider)
		if err != nil {
			s.logger.Warn("resolution switch stop failed", "session_id", liveSession.ID, "provider", provider, "error", err)
			continue
		}
		s.disposeBroadcast(liveSession.UserID, broadcast, phase)
	}
	stoppedAt := time.Now()
	for _, provider := range providers {
		if err := s.sessions.WaitStreamEnded(ctx, liveSession.ID, provider); err != nil {
			s.logger.Warn("resolution switch wait for stop failed", "session_id", liveSession.ID, "provider", provider, "error", err)
		}
	}
	if _, err := s.sessions.ChangeBroadcastResolution(liveSession.ID, resolution); err != nil {
		s.logger.Error("resolution switch change failed", "session_id", liveSession.ID, "error", err)
		for _, provider := range providers {
			fail(provider, "resolution_change_failed")
		}
		liveSession.FinishResolutionSwitch(failures)
		return
	}

	var delayed []string
	for _, provider := range providers {
		if auth.StreamingProvider(provider) == auth.StreamingProviderChzzk {
			delayed = append(delayed, provider)
			continue
		}
		if failure := s.reopenTarget(ctx, liveSession, provider); failure != nil {
			fail(provider, failure.Code)
		}
	}
	if len(delayed) > 0 && s.waitResolutionSwitch(ctx, liveSession, time.Until(stoppedAt.Add(resolutionSwitchChzzkWait))) {
		for _, provider := range delayed {
			if failure := s.reopenTarget(ctx, liveSession, provider); failure != nil {
				fail(provider, failure.Code)
			}
		}
	} else {
		for _, provider := range delayed {
			fail(provider, "resolution_switch_canceled")
		}
	}
	liveSession.FinishResolutionSwitch(failures)
	s.logger.Info("resolution switch finished", "session_id", liveSession.ID, "to", resolution,
		"failed_targets", len(failures), "elapsed_ms", time.Since(started).Milliseconds())
}

// reopenTarget은 대상 하나의 새 방송을 준비하고 라이브로 전환한다. 전환에 실패하면
// 준비한 방송을 치운다 — 준비 상태로 남기면 사용자가 모르는 방송이 채널에 남는다.
func (s *Server) reopenTarget(ctx context.Context, liveSession *session.Session, provider string) *apiError {
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
