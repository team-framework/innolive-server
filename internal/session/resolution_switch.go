package session

import (
	"time"

	"inno-live-server/internal/media"
)

// 방송 중 해상도 전환(#283)의 진행 상태. 서버가 대상을 끝내고 새 해상도로 다시
// 준비·라이브 전환하는 동안(수십 초) 클라이언트는 세션 응답의 이 값으로 진행을 본다.
const (
	ResolutionSwitchSwitching = "switching"
	ResolutionSwitchDone      = "done"
	ResolutionSwitchFailed    = "failed"
	ResolutionSwitchCanceled  = "canceled"
)

// ResolutionSwitchState는 가장 최근 전환의 상태다.
type ResolutionSwitchState struct {
	Status     string   `json:"status"`
	Resolution string   `json:"resolution"`
	Targets    []string `json:"targets"`
	// FailedTargets는 새 방송을 열지 못한 대상과 사유다. 일부만 실패하면 나머지는
	// 라이브로 남고 상태는 done이다. 전부 실패하면 failed다.
	FailedTargets []ResolutionSwitchFailure `json:"failed_targets,omitempty"`
	StartedAt     time.Time                 `json:"started_at"`
	FinishedAt    *time.Time                `json:"finished_at,omitempty"`
}

type ResolutionSwitchFailure struct {
	Provider string `json:"provider"`
	Code     string `json:"code"`
}

// BeginResolutionSwitch는 전환을 시작한다. 이미 진행 중이면 false다.
func (s *Session) BeginResolutionSwitch(resolution string, targets []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolutionSwitch != nil && s.resolutionSwitch.Status == ResolutionSwitchSwitching {
		return false
	}
	// 걸려 있던 화질 올리기 제안은 전환으로 답한 것으로 본다. 보류는 남겨 새
	// 송출이 먼저 쓰게 하고, 전환이 끝나면 호출자가 남은 것을 반납한다(#278).
	if s.upgradeOffer != nil {
		s.clearUpgradeOfferLocked()
	}
	s.resolutionSwitch = &ResolutionSwitchState{
		Status:     ResolutionSwitchSwitching,
		Resolution: resolution,
		Targets:    append([]string(nil), targets...),
		StartedAt:  time.Now().UTC(),
	}
	s.UpdatedAt = time.Now().UTC()
	return true
}

// DiscardResolutionSwitch는 방송을 건드리기 전에 실패한 전환을 없던 일로 한다.
func (s *Session) DiscardResolutionSwitch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolutionSwitch = nil
}

// CancelResolutionSwitch는 진행 중인 전환을 멈추게 한다. 사용자가 방송 종료를
// 누른 경우다 — 전환 작업이 다음 단계에서 이를 보고 새 방송을 열지 않는다.
func (s *Session) CancelResolutionSwitch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolutionSwitch != nil && s.resolutionSwitch.Status == ResolutionSwitchSwitching {
		s.resolutionSwitch.Status = ResolutionSwitchCanceled
		now := time.Now().UTC()
		s.resolutionSwitch.FinishedAt = &now
	}
}

// ResolutionSwitching은 전환이 진행 중인지다.
func (s *Session) ResolutionSwitching() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resolutionSwitch != nil && s.resolutionSwitch.Status == ResolutionSwitchSwitching
}

// ResolutionSwitchCanceled는 진행 중이던 전환이 취소됐는지다.
func (s *Session) ResolutionSwitchCanceled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resolutionSwitch != nil && s.resolutionSwitch.Status == ResolutionSwitchCanceled
}

// FinishResolutionSwitch는 전환을 끝낸다. 취소된 전환은 그대로 둔다.
func (s *Session) FinishResolutionSwitch(failures []ResolutionSwitchFailure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.resolutionSwitch
	if state == nil || state.Status != ResolutionSwitchSwitching {
		return
	}
	state.FailedTargets = failures
	state.Status = ResolutionSwitchDone
	if len(failures) >= len(state.Targets) {
		state.Status = ResolutionSwitchFailed
	}
	now := time.Now().UTC()
	state.FinishedAt = &now
	s.UpdatedAt = now
}

// Done은 세션이 닫히면 닫힌다. 전환 작업의 대기가 세션보다 오래 가지 않게 한다.
func (s *Session) Done() <-chan struct{} { return s.baseCtx.Done() }

func (s *Session) resolutionSwitchSnapshotLocked() *ResolutionSwitchState {
	if s.resolutionSwitch == nil {
		return nil
	}
	snapshot := *s.resolutionSwitch
	snapshot.Targets = append([]string(nil), snapshot.Targets...)
	snapshot.FailedTargets = append([]ResolutionSwitchFailure(nil), snapshot.FailedTargets...)
	return &snapshot
}

// PausedTargets는 라이브 대상 중 일시정지된 대상이다. 해상도 전환(#283)은 새 방송을
// 연 뒤 이 대상들을 다시 멈춘다 — 멈춰 둔 화면이 전환 뒤 저절로 나가면 안 된다.
func (s *Session) PausedTargets() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var paused []string
	for provider, t := range s.targets {
		if t.phase != BroadcastPhaseLive || t.egress == nil || t.stopReason != nil {
			continue
		}
		switch t.egress.Status().Phase {
		case media.EgressPhasePaused, media.EgressPhasePausedReconfiguring, media.EgressPhasePausedReconnecting:
			paused = append(paused, provider)
		}
	}
	return paused
}
