package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/plan"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
)

// 제안은 세션 응답에 실리고, 승낙은 기존 송출 방식 전환으로 한다(#278).
func TestUpgradeOfferAcceptedByModeSwitch(t *testing.T) {
	fixture := newModeSwitchFixture(t, time.Second, plan.Beam, session.Resolution720p, "youtube")
	offer := session.UpgradeOffer{Resolution: session.ResolutionFHD, Mode: plan.ModeFHDSingle, UnitsFrom: 1, UnitsTo: 2, ExpiresAt: time.Now().Add(time.Minute)}
	if err := fixture.manager.OfferUpgrade(fixture.sessionID, offer, time.Minute); err != nil {
		t.Fatal(err)
	}
	payload := getSessionPayload(t, fixture.baseURL, fixture.sessionID, fixture.ownerToken)
	if shown, _ := payload["upgrade_offer"].(map[string]any); shown["mode"] != "fhd_single" || shown["units_to"] != float64(2) {
		t.Fatalf("upgrade_offer = %v, want fhd_single 1→2", payload["upgrade_offer"])
	}

	if status, payload := fixture.putMode(t, `{"resolution":"fhd","targets":["youtube"]}`); status != http.StatusAccepted {
		t.Fatalf("status = %d %v", status, payload)
	}
	final, state := fixture.waitSwitch(t)
	if state["status"] != "done" {
		t.Fatalf("state = %v, want done", state)
	}
	if final["upgrade_offer"] != nil || final["broadcast_resolution"] != session.ResolutionFHD {
		t.Fatalf("after accept offer=%v resolution=%v, want cleared and fhd", final["upgrade_offer"], final["broadcast_resolution"])
	}
	if prepare := fixture.youtube.prepareCount(); prepare != 2 {
		t.Fatalf("youtube prepare calls = %d, want reopened once", prepare)
	}
}

func TestDeleteUpgradeOffer(t *testing.T) {
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{})
	manager.SetPlanResolver(func(context.Context, uuid.UUID) (plan.Plan, error) { return plan.Beam, nil })
	live, ownerToken, err := manager.CreateForUserWithResolution(uuid.New(), session.DefaultProvider, "", session.Resolution720p, nil)
	if err != nil {
		t.Fatal(err)
	}
	decline := func() (int, map[string]any) {
		request, _ := http.NewRequest(http.MethodDelete, server.URL+"/sessions/"+live.ID+"/upgrade-offer", nil)
		request.Header.Set("X-Session-Owner-Token", ownerToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		payload := map[string]any{}
		_ = json.NewDecoder(response.Body).Decode(&payload)
		return response.StatusCode, payload
	}

	if status, payload := decline(); status != http.StatusNotFound || errorCode(payload) != "upgrade_offer_not_found" {
		t.Fatalf("decline without offer = %d %v, want 404 upgrade_offer_not_found", status, payload)
	}
	offer := session.UpgradeOffer{Resolution: session.ResolutionFHD, Mode: plan.ModeFHDSingle, UnitsFrom: 1, UnitsTo: 2}
	if err := manager.OfferUpgrade(live.ID, offer, time.Minute); err != nil {
		t.Fatal(err)
	}
	if status, payload := decline(); status != http.StatusOK || payload["upgrade_offer"] != nil {
		t.Fatalf("decline = %d offer=%v, want 200 and cleared", status, payload["upgrade_offer"])
	}
}

func TestUpgradeCandidateFor(t *testing.T) {
	beamMonthly := func() time.Duration { policy, _ := plan.Beam.Policy(); return policy.MonthlyBroadcast }()
	for _, test := range []struct {
		name       string
		owner      plan.Plan
		resolution string
		targets    int
		used       time.Duration
		opening    bool
		want       plan.Mode
	}{
		{"beam 720p single", plan.Beam, session.Resolution720p, 1, 0, false, plan.ModeFHDSingle},
		{"beam 720p multi has no fhd multi", plan.Beam, session.Resolution720p, 2, 0, false, ""},
		{"plasma 720p multi", plan.Plasma, session.Resolution720p, 2, 0, false, plan.ModeFHDMulti},
		{"spark is not offered", plan.Spark, session.Resolution720p, 1, 0, false, ""},
		{"already fhd", plan.Beam, session.ResolutionFHD, 1, 0, false, ""},
		{"not broadcasting", plan.Beam, session.Resolution720p, 0, 0, false, ""},
		// 새 배수(2)로 30분밖에 남지 않는다.
		{"under an hour at the new rate", plan.Beam, session.Resolution720p, 1, beamMonthly - time.Hour, false, ""},
		// 유튜브는 라이브, 치지직은 아직 열리는 중 — 동시 송출을 단독으로 세면 안 된다.
		{"a target is still opening", plan.Plasma, session.Resolution720p, 1, 0, true, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{})
			manager.SetPlanResolver(func(context.Context, uuid.UUID) (plan.Plan, error) { return test.owner, nil })
			live, _, err := manager.CreateForUserWithResolution(uuid.New(), session.DefaultProvider, "", test.resolution, nil)
			if err != nil {
				t.Fatal(err)
			}
			// 판정은 세션의 대상 상태와 대조한다 — 라이브 대상 수만큼 대상을 연다.
			opened := test.targets
			if test.opening {
				opened++
			}
			for _, provider := range []string{"youtube", "chzzk"}[:opened] {
				if _, err := manager.BeginBroadcastPrepare(live.ID, provider); err != nil {
					t.Fatal(err)
				}
			}
			candidate, ok := upgradeCandidateFor(live, test.targets, 0, test.used)
			if test.want == "" {
				if ok {
					t.Fatalf("candidate = %+v, want none", candidate)
				}
				return
			}
			if !ok || candidate.mode != test.want || candidate.unitsTo != candidate.unitsFrom+1 {
				t.Fatalf("candidate = %+v ok=%v, want %s with one more unit", candidate, ok, test.want)
			}
		})
	}
}

// 자리가 한 유닛뿐이면 늦게 시작했어도 Plasma가 먼저 받는다.
func TestOfferUpgradesPrefersHigherPlan(t *testing.T) {
	cfg := testServerConfig()
	cfg.MaxEgressSlots = 1
	server, manager := newTestApplicationWithConfig(t, cfg, nil)
	plans := map[uuid.UUID]plan.Plan{}
	manager.SetPlanResolver(func(_ context.Context, userID uuid.UUID) (plan.Plan, error) { return plans[userID], nil })
	create := func(owner plan.Plan) *session.Session {
		userID := uuid.New()
		plans[userID] = owner
		live, _, err := manager.CreateForUserWithResolution(userID, session.DefaultProvider, "", session.Resolution720p, nil)
		if err != nil {
			t.Fatal(err)
		}
		return live
	}
	beam := create(plan.Beam)
	time.Sleep(time.Millisecond)
	plasma := create(plan.Plasma)

	var candidates []upgradeCandidate
	for _, live := range []*session.Session{beam, plasma} {
		if _, err := manager.BeginBroadcastPrepare(live.ID, "youtube"); err != nil {
			t.Fatal(err)
		}
		candidate, ok := upgradeCandidateFor(live, 1, 0, 0)
		if !ok {
			t.Fatalf("%s is not a candidate", live.Plan)
		}
		candidates = append(candidates, candidate)
	}
	server.offerUpgrades(candidates, time.Now())

	if plasma.Response().UpgradeOffer == nil {
		t.Fatal("plasma did not get the offer")
	}
	// 자리가 없어 건너뛴 후보는 제안한 것으로 치지 않는다 — 다음 점검에서 다시 본다.
	if beam.Response().UpgradeOffer != nil || beam.UpgradeOffered() {
		t.Fatal("beam must be skipped without being marked as offered")
	}
}
