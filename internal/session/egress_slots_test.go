package session

import (
	"errors"
	"testing"

	"inno-live-server/internal/config"
	"inno-live-server/internal/media"
)

func startableSession(t *testing.T, manager *Manager) *Session {
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

// TestStartStreamWithoutSlotLimit: MAX_EGRESS_SLOTS 미설정 배포는 종전대로
// 제한 없이 시작된다.
func TestStartStreamWithoutSlotLimit(t *testing.T) {
	manager := newTestManager(t, 0)
	for index := 0; index < 3; index++ {
		created := startableSession(t, manager)
		if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/key"); err != nil {
			t.Fatalf("session %d: %v", index, err)
		}
		t.Cleanup(func() { manager.StopStream(created.ID) })
	}
}

// TestStartStreamRejectsWhenSlotsExhausted: 상한을 넘는 송출은 거절된다.
// 상한이 없으면 회선이 포화되어 모든 스트림이 같이 버벅인다.
func TestStartStreamRejectsWhenSlotsExhausted(t *testing.T) {
	manager := newTestManager(t, 0)
	manager.egressSlots = media.NewEgressSlotBudget(1, 0)

	first := startableSession(t, manager)
	if _, err := manager.StartStream(first.ID, "rtmps://a.rtmps.youtube.com/live2/key"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.StopStream(first.ID) })

	second := startableSession(t, manager)
	_, err := manager.StartStream(second.ID, "rtmps://a.rtmps.youtube.com/live2/other")
	if !errors.Is(err, media.ErrEgressSlotsExhausted) {
		t.Fatalf("error = %v, want ErrEgressSlotsExhausted", err)
	}
}

// TestStartStreamReturnsSlotOnValidationFailure: 검증이 실패한 요청이 자리를
// 물고 있으면, 실제로 송출하지 않는 세션이 상한을 갉아먹는다.
func TestStartStreamReturnsSlotOnValidationFailure(t *testing.T) {
	manager := newTestManager(t, 0)
	manager.egressSlots = media.NewEgressSlotBudget(1, 0)

	// 트랙이 없는 세션은 거절된다 — 그때 자리를 돌려놔야 한다.
	created, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/key"); !errors.Is(err, ErrNoVideoTrack) {
		t.Fatalf("error = %v, want ErrNoVideoTrack", err)
	}
	if used := manager.egressSlots.Used(); used != 0 {
		t.Fatalf("Used() = %d, want the slot returned", used)
	}

	// 같은 예산으로 정상 세션이 시작될 수 있어야 한다.
	ok := startableSession(t, manager)
	if _, err := manager.StartStream(ok.ID, "rtmps://a.rtmps.youtube.com/live2/key"); err != nil {
		t.Fatalf("the returned slot must be reusable: %v", err)
	}
	t.Cleanup(func() { manager.StopStream(ok.ID) })
}

// TestStartStreamAssignsNVENCDevice: 카드 배정이 egress까지 전달돼야 -gpu가
// 실린다. 배정이 없으면 전부 GPU 0에 몰린다.
func TestStartStreamAssignsNVENCDevice(t *testing.T) {
	manager := newTestManager(t, 0)
	manager.egressSlots = media.NewEgressSlotBudget(0, 2)
	manager.cfg.EgressVideoEncoder = config.EgressVideoEncoderNVENC

	devices := map[int]bool{}
	for index := 0; index < 2; index++ {
		created := startableSession(t, manager)
		if _, err := manager.StartStream(created.ID, "rtmps://a.rtmps.youtube.com/live2/key"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { manager.StopStream(created.ID) })
		created.mu.RLock()
		devices[created.target(created.Provider).egress.NVENCDevice()] = true
		created.mu.RUnlock()
	}
	if len(devices) != 2 {
		t.Fatalf("two sessions landed on the same card: %v", devices)
	}
}
