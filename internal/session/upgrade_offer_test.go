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
	return UpgradeOffer{UnitsFrom: 1, ExpiresAt: time.Now().Add(time.Minute), Options: []UpgradeOption{
		{Mode: plan.ModeFHDSingle, Resolution: ResolutionFHD, UnitsTo: 2},
	}}
}

// 빈자리에 드는 가장 큰 선택지만큼 보류하고, 보류 안에 드는 선택지만 보인다(#333).
func TestOfferUpgradeHoldsLargestFittingOption(t *testing.T) {
	offer := UpgradeOffer{UnitsFrom: 1, Options: []UpgradeOption{
		{Mode: plan.ModeFHDSingle, Resolution: ResolutionFHD, UnitsTo: 2},
		{Mode: plan.Mode720pMulti, Resolution: Resolution720p, UnitsTo: 2},
		{Mode: plan.ModeFHDMulti, Resolution: ResolutionFHD, UnitsTo: 3},
	}}
	for _, test := range []struct {
		name  string
		units int
		held  int
		shown int
	}{
		// 이 세션은 아직 송출 전이라 빈자리 = 상한이다.
		{"room for all", 2, 2, 3},
		{"room for one more unit", 1, 1, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, live := newUpgradeTestManager(t, test.units)
			if err := manager.OfferUpgrade(live.ID, offer, time.Minute); err != nil {
				t.Fatal(err)
			}
			shown := live.Response().UpgradeOffer
			if held := manager.EgressSlots().Held(live.ID); held != test.held || shown == nil || len(shown.Options) != test.shown {
				t.Fatalf("held=%d offer=%+v, want held %d with %d options", held, shown, test.held, test.shown)
			}
			if shown.Mode != plan.ModeFHDSingle || shown.UnitsTo != 2 {
				t.Fatalf("compat fields = %s %d, want the first option", shown.Mode, shown.UnitsTo)
			}
		})
	}
}

func TestSelectUpgradeOptionExtendsHold(t *testing.T) {
	manager, live := newUpgradeTestManager(t, 2)
	if err := manager.OfferUpgrade(live.ID, testOffer(), 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SelectUpgradeOption(live.ID, plan.ModeFHDMulti, time.Minute); !errors.Is(err, ErrUpgradeOptionNotFound) {
		t.Fatalf("select err = %v, want ErrUpgradeOptionNotFound", err)
	}
	if _, err := manager.SelectUpgradeOption(live.ID, plan.ModeFHDSingle, time.Minute); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if offer := live.Response().UpgradeOffer; offer == nil || offer.Selected != plan.ModeFHDSingle || manager.EgressSlots().Held(live.ID) != 1 {
		t.Fatalf("after the original ttl offer=%+v held=%d, want kept", offer, manager.EgressSlots().Held(live.ID))
	}
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

// 방송 준비·라이브 중인 플랫폼만 사용 중이다. 다른 사용자·다른 플랫폼은 아니다(#348).
func TestProviderInUse(t *testing.T) {
	manager, live := newUpgradeTestManager(t, 0)
	if manager.ProviderInUse(live.UserID, "youtube", uuid.Nil) {
		t.Fatal("idle session must not hold youtube")
	}
	if _, err := manager.BeginBroadcastPrepare(live.ID, "youtube"); err != nil {
		t.Fatal(err)
	}
	if !manager.ProviderInUse(live.UserID, "youtube", uuid.Nil) {
		t.Fatal("preparing youtube must be in use")
	}
	if manager.ProviderInUse(live.UserID, "chzzk", uuid.Nil) || manager.ProviderInUse(uuid.New(), "youtube", uuid.Nil) {
		t.Fatal("other provider or user must not be in use")
	}
}

// 한 채널은 동시에 한 사용자만 송출한다(#406). 다른 사용자의 같은 채널 준비는 거절하고,
// 다른 채널은 막지 않는다. 방송이 끝나면 다시 쓸 수 있다.
func TestBeginBroadcastPrepareOnChannelIsExclusivePerUser(t *testing.T) {
	manager, owner := newUpgradeTestManager(t, 0)
	create := func(userID uuid.UUID) *Session {
		t.Helper()
		live, _, err := manager.CreateForUserWithResolution(userID, DefaultProvider, "", Resolution720p, nil)
		if err != nil {
			t.Fatal(err)
		}
		return live
	}
	other := create(uuid.New())

	if _, _, err := manager.BeginBroadcastPrepareOnChannel(owner.ID, "youtube/UC1", "youtube"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.BeginBroadcastPrepareOnChannel(other.ID, "youtube/UC1", "youtube"); !errors.Is(err, ErrChannelInUseByOtherAccount) {
		t.Fatalf("other user same channel error = %v, want ErrChannelInUseByOtherAccount", err)
	}
	if _, _, err := manager.BeginBroadcastPrepareOnChannel(other.ID, "youtube/UC2", "youtube"); err != nil {
		t.Fatalf("other channel must not be blocked: %v", err)
	}
	manager.ResetBroadcastPreparation(other.ID, "youtube")

	// 선점한 방송이 끝나면(idle) 다른 사용자가 같은 채널을 쓸 수 있다.
	manager.ResetBroadcastPreparation(owner.ID, "youtube")
	if _, _, err := manager.BeginBroadcastPrepareOnChannel(other.ID, "youtube/UC1", "youtube"); err != nil {
		t.Fatalf("channel must be free after the broadcast ends: %v", err)
	}
}
