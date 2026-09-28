package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/plan"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
)

func TestPlanGateError(t *testing.T) {
	cases := []struct {
		plan    plan.Plan
		fhd     bool
		targets int
		code    string
	}{
		{plan.Spark, false, 1, ""},
		{plan.Spark, true, 1, "plan_resolution_not_allowed"},
		{plan.Spark, false, 2, "plan_simulcast_not_allowed"},
		{plan.Beam, true, 1, ""},
		{plan.Beam, false, 2, ""},
		{plan.Beam, true, 2, "plan_simulcast_not_allowed"},
		{plan.Plasma, true, 2, ""},
		{plan.Glow, false, 1, "plan_server_streaming_not_allowed"},
		// 플랜이 없는 세션(인증을 끈 벤치)은 막지 않는다.
		{"", true, 2, ""},
	}
	for _, test := range cases {
		gate := planGateError(test.plan, test.fhd, test.targets)
		got := ""
		if gate != nil {
			got = gate.Code
			if gate.Status != http.StatusForbidden {
				t.Fatalf("%q fhd=%v targets=%d: status %d, want 403", test.plan, test.fhd, test.targets, gate.Status)
			}
		}
		if got != test.code {
			t.Fatalf("%q fhd=%v targets=%d: code %q, want %q", test.plan, test.fhd, test.targets, got, test.code)
		}
	}
}

// 동시 송출 두 번째 대상은 플랫폼을 부르기 전에 플랜으로 거절된다 — 채널에 빈
// 방송이 남지 않는다. 허용되는 플랜은 플랫폼 호출까지 간다.
func TestPrepareGatesSecondTargetByPlan(t *testing.T) {
	for _, test := range []struct {
		plan       plan.Plan
		resolution string
		wantCode   string
		wantCalls  int
	}{
		{plan.Spark, session.Resolution720p, "plan_simulcast_not_allowed", 0},
		{plan.Beam, session.ResolutionFHD, "plan_simulcast_not_allowed", 0},
		{plan.Beam, session.Resolution720p, "", 1},
		{plan.Plasma, session.ResolutionFHD, "", 1},
	} {
		t.Run(string(test.plan)+"_"+test.resolution, func(t *testing.T) {
			chzzk := &stubStreamingProvider{prepareErr: auth.ErrStreamingNotConnected}
			server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
				auth.StreamingProviderYouTube: &stubStreamingProvider{},
				auth.StreamingProviderChzzk:   chzzk,
			})
			manager.SetPlanResolver(func(context.Context, uuid.UUID) (plan.Plan, error) { return test.plan, nil })
			live, ownerToken, err := manager.CreateForUserWithResolution(uuid.New(), session.DefaultProvider, "", test.resolution, nil)
			if err != nil {
				t.Fatal(err)
			}
			// 첫 대상(유튜브)이 이미 준비 중인 상태를 세운다.
			if _, err := manager.BeginBroadcastPrepare(live.ID, "youtube"); err != nil {
				t.Fatal(err)
			}

			response, payload := prepareStream(t, server.URL, live.ID, ownerToken, `{"provider":"chzzk"}`)
			if test.wantCode != "" {
				if response.StatusCode != http.StatusForbidden || streamErrorCode(payload) != test.wantCode {
					t.Fatalf("status=%d code=%q, want 403 %s", response.StatusCode, streamErrorCode(payload), test.wantCode)
				}
			} else if response.StatusCode == http.StatusForbidden {
				t.Fatalf("allowed plan was gated: %v", payload)
			}
			if chzzk.prepareCalls != test.wantCalls {
				t.Fatalf("platform prepare calls = %d, want %d", chzzk.prepareCalls, test.wantCalls)
			}
		})
	}
}

// Spark는 FHD 세션을 만들 수 없다 — 해상도는 세션 생성에서만 고르므로 거기서 막는다.
func TestCreateSessionRejectsFHDForSpark(t *testing.T) {
	requireUser, authenticateUser, header, _, userID := testRequireUser(t)
	application, manager := newTestApplicationWithUserMiddleware(t, requireUser, authenticateUser)
	defer manager.CloseAll()
	store := &memoryPlanStore{plans: map[uuid.UUID]plan.Plan{userID: plan.Spark}}
	application.SetPlanStore(store)
	testServer := httptest.NewServer(application.Handler())
	defer testServer.Close()
	httpServer := testServer.URL

	response := mustRequest(t, http.MethodPost, httpServer+"/sessions", strings.NewReader(`{"broadcast_resolution":"fhd"}`), header)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("spark FHD session status = %d, want 403", response.StatusCode)
	}
	if active, _ := manager.Capacity(); active != 0 {
		t.Fatalf("rejected create left %d sessions", active)
	}

	store.plans[userID] = plan.Beam
	response = mustRequest(t, http.MethodPost, httpServer+"/sessions", strings.NewReader(`{"broadcast_resolution":"fhd"}`), header)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("beam FHD session status = %d, want 201", response.StatusCode)
	}
}
