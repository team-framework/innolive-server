package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/media"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/plan"

	"github.com/google/uuid"
)

func newUpgradeTestManager(t *testing.T, units int) (*Manager, *Session) {
	t.Helper()
	manager, err := NewManager(config.Config{
		PrivacyMode:    config.PrivacyModeBypass,
		FFmpegPath:     "ffmpeg",
		UDPPortMin:     42000,
		UDPPortMax:     42100,
		FrameQueueSize: 2,
		MaxEgressSlots: units,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	manager.SetPlanResolver(func(_ context.Context, _ uuid.UUID) (plan.Plan, error) { return plan.Beam, nil })
	live, _, err := manager.CreateForUserWithResolution(uuid.New(), DefaultProvider, "", Resolution720p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return manager, live
}

func testOffer() UpgradeOffer {
	return UpgradeOffer{Resolution: ResolutionFHD, Mode: plan.ModeFHDSingle, UnitsFrom: 1, UnitsTo: 2, ExpiresAt: time.Now().Add(time.Minute)}
}

func TestOfferUpgradeHoldsUnitsUntilDeclined(t *testing.T) {
	manager, live := newUpgradeTestManager(t, 2)
	if err := manager.OfferUpgrade(live.ID, testOffer(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if held := manager.EgressSlots().Held(live.ID); held != 1 {
		t.Fatalf("held = %d, want 1", held)
	}
	if offer := live.Response().UpgradeOffer; offer == nil || offer.Mode != plan.ModeFHDSingle || offer.UnitsTo != 2 {
		t.Fatalf("response offer = %+v, want fhd_single 1→2", offer)
	}
	// 한 세션에는 한 번만 제안한다.
	if err := manager.OfferUpgrade(live.ID, testOffer(), time.Minute); !errors.Is(err, ErrUpgradeNotOfferable) {
		t.Fatalf("second offer err = %v, want ErrUpgradeNotOfferable", err)
	}

	if _, err := manager.DeclineUpgradeOffer(live.ID); err != nil {
		t.Fatal(err)
	}
	if held := manager.EgressSlots().Held(live.ID); held != 0 || live.Response().UpgradeOffer != nil {
		t.Fatalf("after decline held=%d offer=%v, want released", held, live.Response().UpgradeOffer)
	}
	if _, err := manager.DeclineUpgradeOffer(live.ID); !errors.Is(err, ErrNoUpgradeOffer) {
		t.Fatalf("second decline err = %v, want ErrNoUpgradeOffer", err)
	}
	if err := manager.OfferUpgrade(live.ID, testOffer(), time.Minute); !errors.Is(err, ErrUpgradeNotOfferable) {
		t.Fatalf("offer after decline err = %v, want no re-offer", err)
	}
}

func TestOfferUpgradeFailsWithoutRoom(t *testing.T) {
	manager, live := newUpgradeTestManager(t, 1)
	if err := manager.EgressSlots().Hold(media.EgressClaim{Owner: "other", Group: "beam", Tier: 1}, 1); err != nil {
		t.Fatal(err)
	}
	if err := manager.OfferUpgrade(live.ID, testOffer(), time.Minute); !errors.Is(err, media.ErrEgressSlotsExhausted) {
		t.Fatalf("offer err = %v, want exhausted", err)
	}
	if live.Response().UpgradeOffer != nil || live.UpgradeOffered() {
		t.Fatal("a failed offer must not show or count as offered")
	}
}

func TestOfferUpgradeExpiresAndReleasesHold(t *testing.T) {
	manager, live := newUpgradeTestManager(t, 2)
	if err := manager.OfferUpgrade(live.ID, testOffer(), 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for live.Response().UpgradeOffer != nil || manager.EgressSlots().Held(live.ID) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("offer did not expire and release its hold")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 승낙(전환 시작)은 제안 표시만 치우고 보류는 새 송출이 쓰도록 남긴다.
func TestSwitchStartKeepsHoldAndDeleteReleasesIt(t *testing.T) {
	manager, live := newUpgradeTestManager(t, 2)
	if err := manager.OfferUpgrade(live.ID, testOffer(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if !live.BeginResolutionSwitch(ResolutionFHD, []string{"youtube"}) {
		t.Fatal("BeginResolutionSwitch failed")
	}
	if live.Response().UpgradeOffer != nil || manager.EgressSlots().Held(live.ID) != 1 {
		t.Fatalf("after switch start offer=%v held=%d, want hidden offer and kept hold", live.Response().UpgradeOffer, manager.EgressSlots().Held(live.ID))
	}
	if err := manager.Delete(live.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if held := manager.EgressSlots().Held(live.ID); held != 0 {
		t.Fatalf("held after delete = %d, want 0", held)
	}
}
