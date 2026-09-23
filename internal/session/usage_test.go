package session

import (
	"errors"
	"sync"
	"testing"
	"time"
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
