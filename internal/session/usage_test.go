package session

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeUsageRecorder struct {
	mu     sync.Mutex
	events []UsageEvent
}

func (f *fakeUsageRecorder) RecordUsage(event UsageEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *fakeUsageRecorder) snapshot() []UsageEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]UsageEvent(nil), f.events...)
}

func (f *fakeUsageRecorder) waitFor(t *testing.T, kind UsageEventKind) UsageEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, event := range f.snapshot() {
			if event.Kind == kind {
				return event
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("usage event %q not recorded; got %+v", kind, f.snapshot())
	return UsageEvent{}
}

// TestUsageRecordsSessionAndBroadcastLifecycle: 세션·송출 수명의 각 전이가
// 같은 식별자로 이어지는 사건을 남긴다.
func TestUsageRecordsSessionAndBroadcastLifecycle(t *testing.T) {
	manager := newTestManager(t, 0)
	recorder := &fakeUsageRecorder{}
	manager.SetUsageRecorder(recorder)
	created := startableSession(t, manager)

	if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/key"); err != nil {
		t.Fatal(err)
	}
	// 스트리밍 단계 전의 일시정지는 거절되고, 거절은 기록하지 않는다.
	// 일시정지 시간 누적은 internal/usage의 DB 테스트가 본다.
	if _, err := manager.PauseStream(created.ID); !errors.Is(err, ErrStreamNotActive) {
		t.Fatalf("PauseStream() error = %v, want ErrStreamNotActive", err)
	}
	if _, _, _, err := manager.StopStream(created.ID); err != nil {
		t.Fatal(err)
	}
	ended := recorder.waitFor(t, UsageBroadcastEnded)
	if err := manager.Delete(created.ID, "test_delete"); err != nil {
		t.Fatal(err)
	}

	started := recorder.waitFor(t, UsageSessionStarted)
	if started.SessionID != created.ID || started.At.IsZero() {
		t.Fatalf("session started = %+v", started)
	}
	broadcast := recorder.waitFor(t, UsageBroadcastStarted)
	if broadcast.SessionID != created.ID || broadcast.BroadcastID == "" || broadcast.Provider != DefaultProvider {
		t.Fatalf("broadcast started = %+v", broadcast)
	}
	for _, event := range recorder.snapshot() {
		if event.Kind == UsageBroadcastPaused {
			t.Fatalf("rejected pause was recorded: %+v", event)
		}
	}
	if ended.BroadcastID != broadcast.BroadcastID || ended.Reason != "user_requested" {
		t.Fatalf("broadcast ended = %+v", ended)
	}
	sessionEnded := recorder.waitFor(t, UsageSessionEnded)
	if sessionEnded.SessionID != created.ID || sessionEnded.Reason != "test_delete" {
		t.Fatalf("session ended = %+v", sessionEnded)
	}
}

// TestUsageEndsBroadcastWhenSessionCloses: 송출 중에 세션이 끝나면 송출
// 종료도 세션 종료 사유로 남는다 — 종료 로그가 비던 경로다.
func TestUsageEndsBroadcastWhenSessionCloses(t *testing.T) {
	manager := newTestManager(t, 0)
	recorder := &fakeUsageRecorder{}
	manager.SetUsageRecorder(recorder)
	created := startableSession(t, manager)
	if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/key"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Delete(created.ID, "peer_connection_closed"); err != nil {
		t.Fatal(err)
	}
	if ended := recorder.waitFor(t, UsageBroadcastEnded); ended.Reason != "session_closed" {
		t.Fatalf("broadcast end reason = %q, want session_closed", ended.Reason)
	}
}

// TestUsageSkipsRejectedStreamStart: 거절된 송출 시작은 방송으로 세지 않는다.
func TestUsageSkipsRejectedStreamStart(t *testing.T) {
	manager := newTestManager(t, 0)
	recorder := &fakeUsageRecorder{}
	manager.SetUsageRecorder(recorder)
	created, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/key"); !errors.Is(err, ErrNoVideoTrack) {
		t.Fatalf("StartStream() error = %v, want ErrNoVideoTrack", err)
	}
	for _, event := range recorder.snapshot() {
		if event.Kind == UsageBroadcastStarted {
			t.Fatalf("rejected start recorded a broadcast: %+v", event)
		}
	}
}

func liveOnDeviceTarget(t *testing.T, manager *Manager) *Session {
	t.Helper()
	created, _, err := manager.CreateForUserWithAIProcessing(uuid.New(), DefaultProvider, AIProcessingOnDevice, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginBroadcastPrepare(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.MarkBroadcastPrepared(created.ID, PlatformBroadcast{Provider: DefaultProvider, BroadcastID: "b1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginGoLive(created.ID); err != nil {
		t.Fatal(err)
	}
	if aborted, _, err := manager.CompleteGoLive(created.ID); aborted || err != nil {
		t.Fatalf("CompleteGoLive() aborted=%v err=%v", aborted, err)
	}
	return created
}

// TestUsageRecordsOnDeviceDirectBroadcast: 앱이 직접 송출하는 대상은 egress가
// 없지만 라이브 전환부터 중지까지 한 송출 세대로 기록된다(#435).
func TestUsageRecordsOnDeviceDirectBroadcast(t *testing.T) {
	manager := newTestManager(t, 0)
	recorder := &fakeUsageRecorder{}
	manager.SetUsageRecorder(recorder)
	created := liveOnDeviceTarget(t, manager)

	if _, _, _, err := manager.StopStreamWithReason(created.ID, "broadcast_limit_reached"); err != nil {
		t.Fatal(err)
	}
	started := recorder.waitFor(t, UsageBroadcastStarted)
	live := recorder.waitFor(t, UsageBroadcastLive)
	ended := recorder.waitFor(t, UsageBroadcastEnded)
	if started.SessionID != created.ID || started.Provider != DefaultProvider || started.BroadcastID == "" {
		t.Fatalf("broadcast started = %+v", started)
	}
	if live.BroadcastID != started.BroadcastID || ended.BroadcastID != started.BroadcastID || ended.Reason != "broadcast_limit_reached" {
		t.Fatalf("live = %+v, ended = %+v", live, ended)
	}
}

// TestUsageClosesOnDeviceBroadcastOnSessionDelete: 중지 없이 세션이 끝나도
// 직접 송출 세대가 열린 채 남지 않는다 — 남으면 차감이 now까지 계속 쌓인다.
func TestUsageClosesOnDeviceBroadcastOnSessionDelete(t *testing.T) {
	manager := newTestManager(t, 0)
	recorder := &fakeUsageRecorder{}
	manager.SetUsageRecorder(recorder)
	created := liveOnDeviceTarget(t, manager)

	if err := manager.Delete(created.ID, "test_delete"); err != nil {
		t.Fatal(err)
	}
	ended := recorder.waitFor(t, UsageBroadcastEnded)
	if ended.Reason != "session_closed" {
		t.Fatalf("broadcast ended = %+v", ended)
	}
}

// TestUsageServerModeGoLiveWithoutEgressOpensNothing: server 모드는 egress가
// 송출 세대를 연다. egress 없이 전환만 된 경우 세대를 새로 열지 않는다.
func TestUsageServerModeGoLiveWithoutEgressOpensNothing(t *testing.T) {
	manager := newTestManager(t, 0)
	recorder := &fakeUsageRecorder{}
	manager.SetUsageRecorder(recorder)
	created, _, err := manager.CreateForUserWithAIProcessing(uuid.New(), DefaultProvider, AIProcessingServer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginBroadcastPrepare(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.MarkBroadcastPrepared(created.ID, PlatformBroadcast{Provider: DefaultProvider, BroadcastID: "b1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginGoLive(created.ID); err != nil {
		t.Fatal(err)
	}
	manager.CompleteGoLive(created.ID)
	for _, event := range recorder.snapshot() {
		if event.Kind == UsageBroadcastStarted || event.Kind == UsageBroadcastLive {
			t.Fatalf("server mode go live without egress recorded %+v", event)
		}
	}
}
