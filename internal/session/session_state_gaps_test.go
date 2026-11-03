package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"inno-live-server/internal/media"
	"inno-live-server/internal/metrics"

	"github.com/google/uuid"
)

// 라이브 전환 왕복 중에 egress가 재연결 예산을 소진해 끝나면 runEgress는 방송을
// 정리하지 않는다(전환 쪽이 마무리한다). 전환이 성공했다고 live로 올리면 송출 없는
// live에 갇혀 설정 변경이 막히고 플랫폼 방송도 남는다(P1-8).
func TestCompleteGoLiveAbortsWhenEgressAlreadyStopped(t *testing.T) {
	manager := newTestManager(t, 0)
	live, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	egress := media.NewRTMPEgress("ffmpeg", slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), media.TranscoderOptions{}, "rtmp://example.invalid/live/key", nil, false, 0, "", "")
	egress.Stop()

	live.mu.Lock()
	target := live.target(DefaultProvider)
	target.egress = egress
	target.phase = BroadcastPhaseGoingLive
	target.platformBroadcast = &PlatformBroadcast{BroadcastID: "broadcast-1"}
	live.mu.Unlock()

	aborted, broadcast, err := manager.CompleteGoLive(live.ID, DefaultProvider)
	if !aborted || broadcast.BroadcastID != "broadcast-1" {
		t.Fatalf("CompleteGoLive = aborted %v, broadcast %q, err %v; want aborted with the broadcast to end", aborted, broadcast.BroadcastID, err)
	}
	live.mu.RLock()
	defer live.mu.RUnlock()
	if target.phase != BroadcastPhaseIdle || target.platformBroadcast != nil {
		t.Fatalf("target after abort = phase %q, broadcast %v; want idle without broadcast", target.phase, target.platformBroadcast)
	}
}

// 라이브 전환 왕복 중에 저장한 설정은 이미 나간 전환 요청에 반영되지 않는다.
// 준비·라이브 중과 같이 거절해야 조회 결과와 실제 방송이 어긋나지 않는다(P1-9).
func TestBroadcastSettingsRejectedWhileGoingLive(t *testing.T) {
	manager := newTestManager(t, 0)
	live, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{DefaultProvider, "chzzk"} {
		live.mu.Lock()
		live.target(provider).phase = BroadcastPhaseGoingLive
		live.mu.Unlock()
	}
	madeForKids := false
	if _, err := manager.SetBroadcastSettings(live.ID, YouTubeBroadcastSettings{Title: "전환 중", MadeForKids: &madeForKids}); !errors.Is(err, ErrBroadcastLive) {
		t.Fatalf("YouTube settings while going live = %v, want ErrBroadcastLive", err)
	}
	if _, err := manager.SetChzzkBroadcastSettings(live.ID, ChzzkBroadcastSettings{Title: "전환 중"}, "chzzk"); !errors.Is(err, ErrBroadcastLive) {
		t.Fatalf("CHZZK settings while going live = %v, want ErrBroadcastLive", err)
	}
}

// 이관 재개에서 StartStream이 실패하면(예: 만석) 대상이 egress 없는 live로 남는다.
// 서버 처리 세션의 중지가 egress만 보고 거절하면 그 방송은 멈출 길이 없다(P1-38).
func TestStopReleasesLiveTargetLeftWithoutEgress(t *testing.T) {
	manager := newTestManager(t, 0)
	live, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	live.mu.Lock()
	target := live.target(DefaultProvider)
	target.phase = BroadcastPhaseLive
	target.platformBroadcast = &PlatformBroadcast{BroadcastID: "broadcast-1"}
	live.mu.Unlock()

	_, broadcast, phase, err := manager.StopStream(live.ID, DefaultProvider)
	if err != nil || broadcast.BroadcastID != "broadcast-1" || phase != BroadcastPhaseLive {
		t.Fatalf("StopStream = broadcast %q, phase %q, err %v; want the live broadcast released", broadcast.BroadcastID, phase, err)
	}
}

// 송출 자리를 검증보다 먼저 잡으면, 만석일 때 트랙이 없는 요청도 자리를 기다리다
// 만석 오류(503)로 끝나고 실제 원인(409)이 가려진다. 검증에 걸릴 요청이 잠깐이라도
// 자리를 쥐면 정상 요청이 만석으로 밀릴 수도 있다(P2-8).
func TestStartStreamValidatesBeforeClaimingSlot(t *testing.T) {
	manager, live := newUpgradeTestManager(t, 1)
	if err := manager.egressSlots.Hold(media.EgressClaim{Owner: "someone-else"}, 1); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := manager.StartStream(live.ID, "rtmp://example.invalid/live/key")
	if !errors.Is(err, ErrNoVideoTrack) {
		t.Fatalf("StartStream without a track on full slots = %v, want ErrNoVideoTrack", err)
	}
	if waited := time.Since(started); waited > 500*time.Millisecond {
		t.Fatalf("StartStream waited %s for a slot before failing validation", waited)
	}
}

// 온디바이스 직접 송출은 egress가 없어 송출 세대를 서버가 닫아야 한다. 탈퇴 경로가
// 닫지 않으면 원장에 열린 송출이 남는다(P1-35).
func TestWithdrawalEndsDirectBroadcastUsage(t *testing.T) {
	manager := newTestManager(t, 0)
	usage := &recordedUsage{}
	manager.SetUsageRecorder(usage)
	userID := uuid.New()
	live, _, err := manager.CreateForUser(userID, nil)
	if err != nil {
		t.Fatal(err)
	}
	live.mu.Lock()
	live.AIProcessing = AIProcessingOnDevice
	target := live.target(DefaultProvider)
	target.phase = BroadcastPhaseLive
	target.usageID = "direct-usage"
	live.mu.Unlock()

	if err := manager.CloseUserSessionsForWithdrawal(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	usage.mu.Lock()
	defer usage.mu.Unlock()
	for _, event := range usage.events {
		if event.Kind == UsageBroadcastEnded && event.BroadcastID == "direct-usage" {
			return
		}
	}
	t.Fatalf("usage events = %+v, want the direct broadcast ended", usage.events)
}
