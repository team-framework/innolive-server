package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
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
	if err := fixture.manager.OfferUpgrade(fixture.sessionID, fhdSingleOffer(), time.Minute); err != nil {
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
	if err := manager.OfferUpgrade(live.ID, fhdSingleOffer(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if status, payload := decline(); status != http.StatusOK || payload["upgrade_offer"] != nil {
		t.Fatalf("decline = %d offer=%v, want 200 and cleared", status, payload["upgrade_offer"])
	}
}

func fhdSingleOffer() session.UpgradeOffer {
	return session.UpgradeOffer{UnitsFrom: 1, ExpiresAt: time.Now().Add(time.Minute), Options: []session.UpgradeOption{
		{Mode: plan.ModeFHDSingle, Resolution: session.ResolutionFHD, Targets: []string{"youtube"}, UnitsTo: 2, RestartsBroadcast: true},
		{Mode: plan.Mode720pMulti, Resolution: session.Resolution720p, Targets: []string{"youtube", "chzzk"}, UnitsTo: 2, NeedsSettings: true},
	}}
}

// 선택지를 고르면 보류가 늘고, 제안에 없는 선택지는 거절한다(#333).
func TestSelectUpgradeOption(t *testing.T) {
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{})
	manager.SetPlanResolver(func(context.Context, uuid.UUID) (plan.Plan, error) { return plan.Beam, nil })
	live, ownerToken, err := manager.CreateForUserWithResolution(uuid.New(), session.DefaultProvider, "", session.Resolution720p, nil)
	if err != nil {
		t.Fatal(err)
	}
	selectMode := func(mode string) (int, map[string]any) {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/sessions/"+live.ID+"/upgrade-offer/select", strings.NewReader(`{"mode":"`+mode+`"}`))
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
	if status, payload := selectMode("720p_multi"); status != http.StatusNotFound || errorCode(payload) != "upgrade_offer_not_found" {
		t.Fatalf("select without offer = %d %v", status, payload)
	}
	if err := manager.OfferUpgrade(live.ID, fhdSingleOffer(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if status, payload := selectMode("fhd_multi"); status != http.StatusBadRequest || errorCode(payload) != "upgrade_option_not_found" {
		t.Fatalf("select missing option = %d %v", status, payload)
	}
	status, payload := selectMode("720p_multi")
	if status != http.StatusOK {
		t.Fatalf("select = %d %v", status, payload)
	}
	offer, _ := payload["upgrade_offer"].(map[string]any)
	expires, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(offer["expires_at"]))
	if offer["selected"] != "720p_multi" || time.Until(expires) < 2*time.Minute {
		t.Fatalf("offer = %v, want selected with a ~3 minute hold", offer)
	}
}

// 선택지 표(#333): 지금보다 유닛이 많고 해상도·대상 수가 줄지 않는 허용 방식 전부.
// 동시 송출은 추가할 계정이 연결돼 있을 때만.
func TestUpgradeCandidateFor(t *testing.T) {
	beamMonthly := func() time.Duration { policy, _ := plan.Beam.Policy(); return policy.MonthlyBroadcast }()
	chzzk := []string{"chzzk"}
	for _, test := range []struct {
		name       string
		owner      plan.Plan
		resolution string
		targets    int
		addable    []string
		used       time.Duration
		opening    bool
		want       []plan.Mode
	}{
		{"beam 720p single", plan.Beam, session.Resolution720p, 1, chzzk, 0, false, []plan.Mode{plan.ModeFHDSingle, plan.Mode720pMulti}},
		{"beam 720p single without another account", plan.Beam, session.Resolution720p, 1, nil, 0, false, []plan.Mode{plan.ModeFHDSingle}},
		{"beam fhd single", plan.Beam, session.ResolutionFHD, 1, chzzk, 0, false, nil},
		{"beam 720p multi has no fhd multi", plan.Beam, session.Resolution720p, 2, nil, 0, false, nil},
		{"plasma 720p single", plan.Plasma, session.Resolution720p, 1, chzzk, 0, false, []plan.Mode{plan.ModeFHDSingle, plan.Mode720pMulti, plan.ModeFHDMulti}},
		{"plasma fhd single", plan.Plasma, session.ResolutionFHD, 1, chzzk, 0, false, []plan.Mode{plan.ModeFHDMulti}},
		{"plasma 720p multi", plan.Plasma, session.Resolution720p, 2, nil, 0, false, []plan.Mode{plan.ModeFHDMulti}},
		{"spark is not offered", plan.Spark, session.Resolution720p, 1, chzzk, 0, false, nil},
		{"not broadcasting", plan.Beam, session.Resolution720p, 0, chzzk, 0, false, nil},
		// 새 배수(2)로 30분밖에 남지 않는다.
		{"under an hour at the new rate", plan.Beam, session.Resolution720p, 1, chzzk, beamMonthly - time.Hour, false, nil},
		// 유튜브는 라이브, 치지직은 아직 열리는 중 — 동시 송출을 단독으로 세면 안 된다.
		{"a target is still opening", plan.Plasma, session.Resolution720p, 1, nil, 0, true, nil},
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
			candidate, ok := upgradeCandidateFor(live, []string{"youtube", "chzzk"}[:test.targets], test.addable, 0, test.used)
			if test.want == nil {
				if ok {
					t.Fatalf("candidate = %+v, want none", candidate)
				}
				return
			}
			var modes []plan.Mode
			for _, option := range candidate.options {
				modes = append(modes, option.Mode)
			}
			if !ok || !slices.Equal(modes, test.want) {
				t.Fatalf("options = %v ok=%v, want %v", modes, ok, test.want)
			}
		})
	}
}

// 해상도가 바뀌는 선택지만 재시작이고, 대상별 영향을 싣는다. 플랫폼을 더하는
// 선택지는 설정이 필요하다.
func TestUpgradeOptionsDescribeRestart(t *testing.T) {
	_, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{})
	manager.SetPlanResolver(func(context.Context, uuid.UUID) (plan.Plan, error) { return plan.Plasma, nil })
	live, _, err := manager.CreateForUserWithResolution(uuid.New(), session.DefaultProvider, "", session.Resolution720p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginBroadcastPrepare(live.ID, "chzzk"); err != nil {
		t.Fatal(err)
	}
	candidate, ok := upgradeCandidateFor(live, []string{"chzzk"}, []string{"youtube"}, 0, 0)
	if !ok {
		t.Fatal("no candidate")
	}
	byMode := map[plan.Mode]session.UpgradeOption{}
	for _, option := range candidate.options {
		byMode[option.Mode] = option
	}
	fhd := byMode[plan.ModeFHDSingle]
	if !fhd.RestartsBroadcast || fhd.NeedsSettings || len(fhd.RestartEffects) != 1 ||
		fhd.RestartEffects[0] != (session.RestartEffect{Provider: "chzzk", SameLink: true, GapSeconds: 15}) {
		t.Fatalf("fhd single = %+v", fhd)
	}
	multi := byMode[plan.Mode720pMulti]
	if multi.RestartsBroadcast || !multi.NeedsSettings || len(multi.RestartEffects) != 0 || !slices.Equal(multi.Targets, []string{"chzzk", "youtube"}) || multi.UnitsTo != 2 {
		t.Fatalf("720p multi = %+v", multi)
	}
	both := byMode[plan.ModeFHDMulti]
	if !both.RestartsBroadcast || !both.NeedsSettings || both.UnitsTo != 3 {
		t.Fatalf("fhd multi = %+v", both)
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
		candidate, ok := upgradeCandidateFor(live, []string{"youtube"}, nil, 0, 0)
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
