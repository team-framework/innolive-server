package session

import (
	"testing"

	"inno-live-server/internal/media"
)

// OfferUpgrade가 세션을 찾은 직후 Delete가 세션을 지우고 보류를 반납하면, 그 뒤에
// 잡힌 보류는 세션 없이 남는다. 제안 만료가 세션을 못 찾아도 보류는 반납해야
// 유닛이 영구히 새지 않는다(P1-3).
func TestExpiredOfferReleasesHoldAfterSessionDeleted(t *testing.T) {
	manager, live := newUpgradeTestManager(t, 2)
	offer := testOffer()
	if err := manager.Delete(live.ID, "test"); err != nil {
		t.Fatal(err)
	}
	// Delete와 경합한 OfferUpgrade가 늦게 잡은 보류.
	if err := manager.egressSlots.Hold(media.EgressClaim{Owner: live.ID, Tier: egressTierFor(live.Plan), Group: egressGroupFor(live.Plan)}, 1); err != nil {
		t.Fatal(err)
	}

	manager.expireUpgradeOffer(live.ID, &offer)

	if held := manager.EgressSlots().Held(live.ID); held != 0 {
		t.Fatalf("held after the deleted session's offer expired = %d, want 0", held)
	}
}
