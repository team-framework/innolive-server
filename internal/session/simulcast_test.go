package session

import (
	"testing"

	"inno-live-server/internal/media"
)

// trackedSession은 egress를 붙일 수 있는 세션 하나를 만든다. 실제 트랙 협상
// 없이 송출 경로만 보는 테스트가 쓴다.
func trackedSession(t *testing.T, manager *Manager) *Session {
	t.Helper()
	created, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	created.mu.Lock()
	created.rawTrackID = "video-track"
	created.mu.Unlock()
	return created
}

func targetEgress(t *testing.T, s *Session, provider string) *media.RTMPEgress {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readTarget(provider).egress
}

// TestStartStreamRunsTargetsSideBySide: 한 세션이 두 플랫폼으로 동시에 나간다.
// 대상마다 자기 egress를 들고, 영상 팬아웃에도 둘 다 올라가야 한다.
func TestStartStreamRunsTargetsSideBySide(t *testing.T) {
	manager := newTestManager(t, 0)
	created := trackedSession(t, manager)

	if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/key", StreamOptions{Provider: "youtube"}); err != nil {
		t.Fatalf("youtube StartStream() error = %v", err)
	}
	if _, err := manager.StartStream(created.ID, "rtmp://rtmp.stream.naver.com/relay/key", StreamOptions{Provider: "chzzk"}); err != nil {
		t.Fatalf("chzzk StartStream() error = %v", err)
	}
	t.Cleanup(func() {
		manager.StopStream(created.ID, "youtube")
		manager.StopStream(created.ID, "chzzk")
	})

	youtube := targetEgress(t, created, "youtube")
	chzzk := targetEgress(t, created, "chzzk")
	if youtube == nil || chzzk == nil {
		t.Fatalf("egress = %v/%v, want both targets streaming", youtube, chzzk)
	}
	if youtube == chzzk {
		t.Fatal("targets share one egress, want independent processes")
	}
	if sinks := len(created.egressFanout.Sinks()); sinks != 2 {
		t.Fatalf("fanout sinks = %d, want 2", sinks)
	}
	// 응답도 대상 둘을 정렬 순으로 싣는다.
	targets := created.Response().Targets
	if len(targets) != 2 || targets[0].Provider != "chzzk" || targets[1].Provider != "youtube" {
		t.Fatalf("targets = %+v, want chzzk then youtube", targets)
	}
}

// TestStartStreamRejectsSecondEgressOnSameTarget: 대상 단위 가드는 남아 있어야
// 한다 — 같은 플랫폼에 egress를 겹쳐 붙이면 방송이 둘이 된다.
func TestStartStreamRejectsSecondEgressOnSameTarget(t *testing.T) {
	manager := newTestManager(t, 0)
	created := trackedSession(t, manager)

	if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/key", StreamOptions{Provider: "youtube"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.StopStream(created.ID, "youtube") })

	if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/key2", StreamOptions{Provider: "youtube"}); err != ErrStreamActive {
		t.Fatalf("second StartStream() error = %v, want ErrStreamActive", err)
	}
}

// TestStopStreamLeavesOtherTargetStreaming: 개별 종료 — 한쪽을 끝내도 다른
// 쪽은 라이브로 남는다. 동시 송출의 실패 정책(나머지 진행)이 기대는 성질이다.
func TestStopStreamLeavesOtherTargetStreaming(t *testing.T) {
	manager := newTestManager(t, 0)
	created := trackedSession(t, manager)

	if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/key", StreamOptions{Provider: "youtube"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartStream(created.ID, "rtmp://rtmp.stream.naver.com/relay/key", StreamOptions{Provider: "chzzk"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.StopStream(created.ID, "youtube") })

	if _, _, _, err := manager.StopStream(created.ID, "chzzk"); err != nil {
		t.Fatalf("chzzk StopStream() error = %v", err)
	}
	if sinks := len(created.egressFanout.Sinks()); sinks != 1 {
		t.Fatalf("fanout sinks = %d, want the surviving target only", sinks)
	}
	if egress := targetEgress(t, created, "youtube"); egress == nil || egress.Status().Phase == media.EgressPhaseStopped {
		t.Fatal("youtube target must keep streaming after the chzzk target stops")
	}
	// 남은 대상은 다시 중지할 수 있고, 이미 끝난 대상은 활성이 아니다.
	if _, _, _, err := manager.StopStream(created.ID, "chzzk"); err != ErrStreamNotActive {
		t.Fatalf("second chzzk StopStream() error = %v, want ErrStreamNotActive", err)
	}
}

// TestAllTargetsPausedRequiresEveryTarget: AI 입력은 세션에 하나뿐이라 대상
// 하나를 멈춘다고 끊으면 나머지 대상 시청자의 화면까지 정지한다. 송출 중인
// 대상이 전부 멈춘 뒤에만 차단해야 한다.
func TestAllTargetsPausedRequiresEveryTarget(t *testing.T) {
	cases := []struct {
		name   string
		phases []media.EgressPhase
		want   bool
	}{
		{"한쪽만 멈춤", []media.EgressPhase{media.EgressPhasePaused, media.EgressPhaseStreaming}, false},
		{"전부 멈춤", []media.EgressPhase{media.EgressPhasePaused, media.EgressPhasePausedReconnecting}, true},
		{"단독 송출 멈춤", []media.EgressPhase{media.EgressPhasePaused}, true},
		{"단독 송출 중", []media.EgressPhase{media.EgressPhaseStreaming}, false},
		{"송출 중인 대상 없음", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allTargetsPaused(tc.phases); got != tc.want {
				t.Fatalf("allTargetsPaused(%v) = %v, want %v", tc.phases, got, tc.want)
			}
		})
	}
}
