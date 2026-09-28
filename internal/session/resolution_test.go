package session

import (
	"errors"
	"testing"

	"inno-live-server/internal/media"

	"github.com/google/uuid"
)

func TestPinLongEdgeFor(t *testing.T) {
	tests := []struct {
		resolution string
		globalPin  int
		want       int
	}{
		{Resolution720p, 1280, 1280},
		{ResolutionFHD, 1280, 1920},
		// 전역 핀이 꺼진 배포(로컬·벤치)는 해상도와 무관하게 종전대로 핀 없음.
		{Resolution720p, 0, 0},
		{ResolutionFHD, 0, 0},
	}
	for _, test := range tests {
		if got := pinLongEdgeFor(test.resolution, test.globalPin); got != test.want {
			t.Fatalf("pinLongEdgeFor(%q, %d) = %d, want %d", test.resolution, test.globalPin, got, test.want)
		}
	}
}

func TestCreateFixesResolutionPerSession(t *testing.T) {
	manager := newTestManager(t, 0)
	manager.cfg.DecoderPinLongEdge = 1280

	fhd, _, err := manager.CreateForUserWithResolution(uuid.New(), DefaultProvider, "", ResolutionFHD, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy, _, err := manager.CreateForUserWithAIProcessing(uuid.New(), DefaultProvider, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fhd.BroadcastResolution != ResolutionFHD || fhd.pinLongEdge != 1920 {
		t.Fatalf("fhd session = %q pin %d", fhd.BroadcastResolution, fhd.pinLongEdge)
	}
	// 해상도를 보내지 않는 기존 클라이언트는 720p다.
	if legacy.BroadcastResolution != Resolution720p || legacy.pinLongEdge != 1280 {
		t.Fatalf("legacy session = %q pin %d", legacy.BroadcastResolution, legacy.pinLongEdge)
	}
	if got := fhd.Response().BroadcastResolution; got != ResolutionFHD {
		t.Fatalf("response resolution = %q", got)
	}
}

func TestCreateRejectsUnknownResolution(t *testing.T) {
	manager := newTestManager(t, 1)
	for _, value := range []string{"1080p", "FHD", "4k"} {
		if _, _, err := manager.CreateForUserWithResolution(uuid.New(), DefaultProvider, "", value, nil); !errors.Is(err, ErrInvalidResolution) {
			t.Fatalf("%q: error = %v, want ErrInvalidResolution", value, err)
		}
	}
	if active, _ := manager.Capacity(); active != 0 {
		t.Fatalf("rejected creates left %d sessions", active)
	}
}

// 해상도 변경(#283): 송출 전에는 바뀌고 핀·사용 기록이 따라간다. 같은 값은 아무것도
// 하지 않고, 미지원 값은 거절한다.
func TestChangeBroadcastResolution(t *testing.T) {
	manager := newTestManager(t, 0)
	manager.cfg.DecoderPinLongEdge = 1280
	recorder := &fakeUsageRecorder{}
	manager.SetUsageRecorder(recorder)
	live, _, err := manager.CreateForUserWithResolution(uuid.New(), DefaultProvider, "", Resolution720p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ChangeBroadcastResolution(live.ID, "1080p"); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("unsupported value error = %v", err)
	}
	if _, err := manager.ChangeBroadcastResolution(live.ID, ResolutionFHD); err != nil {
		t.Fatal(err)
	}
	live.mu.RLock()
	pin := live.pinLongEdge
	live.mu.RUnlock()
	if live.Resolution() != ResolutionFHD || pin != 1920 || live.Response().BroadcastResolution != ResolutionFHD {
		t.Fatalf("after change: resolution %q pin %d", live.Resolution(), pin)
	}
	event := recorder.waitFor(t, UsageResolutionChanged)
	if event.SessionID != live.ID || event.Resolution != ResolutionFHD {
		t.Fatalf("usage event = %+v", event)
	}
	if _, err := manager.ChangeBroadcastResolution(live.ID, ResolutionFHD); err != nil {
		t.Fatal(err)
	}
	changes := 0
	for _, event := range recorder.snapshot() {
		if event.Kind == UsageResolutionChanged {
			changes++
		}
	}
	if changes != 1 {
		t.Fatalf("resolution change events = %d, want 1 (same value is a no-op)", changes)
	}
	if _, err := manager.ChangeBroadcastResolution("missing", ResolutionFHD); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session error = %v", err)
	}
}

// 멈추지 않은 송출이 있으면 거절하고 유닛·해상도를 그대로 둔다. 이 테스트의 egress는
// 프레임이 없어 idle이다 — 멈춘 상태가 아니다.
func TestChangeBroadcastResolutionRejectsUnpausedStream(t *testing.T) {
	manager := newTestManager(t, 0)
	manager.egressSlots = media.NewEgressSlotBudget(4, 0)
	live := startableSession(t, manager)
	if _, err := manager.StartStream(live.ID, "rtmps://a.rtmps.youtube.com/live2/key"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.StopStream(live.ID) })
	if _, err := manager.ChangeBroadcastResolution(live.ID, ResolutionFHD); !errors.Is(err, ErrResolutionChangeWhileLive) {
		t.Fatalf("error = %v, want ErrResolutionChangeWhileLive", err)
	}
	if live.Resolution() != Resolution720p || manager.egressSlots.Used() != 1 {
		t.Fatalf("after rejection: resolution %q units %d, want 720p / 1", live.Resolution(), manager.egressSlots.Used())
	}
}

// 송출 전 FHD로 바꾸면 다음 송출이 FHD 유닛(2)으로 잡힌다 — 자리가 1뿐이면 못 연다.
func TestChangedResolutionAppliesToNextStream(t *testing.T) {
	manager := newTestManager(t, 0)
	manager.egressSlots = media.NewEgressSlotBudget(1, 0)
	live := startableSession(t, manager)
	if _, err := manager.ChangeBroadcastResolution(live.ID, ResolutionFHD); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartStream(live.ID, "rtmps://a.rtmps.youtube.com/live2/key"); !errors.Is(err, media.ErrEgressSlotsExhausted) {
		t.Fatalf("FHD start with one unit = %v, want exhausted", err)
	}
	if used := manager.egressSlots.Used(); used != 0 {
		t.Fatalf("units after failed start = %d, want 0", used)
	}
}

func TestAnyTargetUnpaused(t *testing.T) {
	cases := []struct {
		name   string
		phases []media.EgressPhase
		want   bool
	}{
		{"송출 전", nil, false},
		{"전부 멈춤", []media.EgressPhase{media.EgressPhasePaused, media.EgressPhasePausedReconnecting}, false},
		{"멈춤 + 끝남", []media.EgressPhase{media.EgressPhasePausedReconfiguring, media.EgressPhaseStopped}, false},
		{"한쪽만 멈춤", []media.EgressPhase{media.EgressPhasePaused, media.EgressPhaseStreaming}, true},
		{"재연결 중", []media.EgressPhase{media.EgressPhaseReconnecting}, true},
		{"프레임 대기(idle)", []media.EgressPhase{media.EgressPhaseIdle}, true},
	}
	for _, test := range cases {
		if got := anyTargetUnpaused(test.phases); got != test.want {
			t.Fatalf("%s: got %v, want %v", test.name, got, test.want)
		}
	}
}
