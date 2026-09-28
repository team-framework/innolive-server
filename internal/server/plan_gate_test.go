package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// 같은 대상을 다시 준비하려는 요청은 플랜 판정이 아니라 기존 계약(409)을 받아야
// 한다 — 자기 자신을 "다른 대상"으로 세면 동시 송출로 오판한다.
func TestPrepareSameTargetAgainKeeps409(t *testing.T) {
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: &stubStreamingProvider{},
	})
	manager.SetPlanResolver(func(context.Context, uuid.UUID) (plan.Plan, error) { return plan.Spark, nil })
	live, ownerToken, err := manager.CreateForUserWithResolution(uuid.New(), session.DefaultProvider, "", session.Resolution720p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginBroadcastPrepare(live.ID, "youtube"); err != nil {
		t.Fatal(err)
	}
	response, payload := prepareStream(t, server.URL, live.ID, ownerToken, `{"provider":"youtube"}`)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d code=%q, want 409 (not a plan rejection)", response.StatusCode, streamErrorCode(payload))
	}
}

// 거절된 준비는 선점을 되돌려야 한다 — 남으면 그 대상이 영영 "준비 중"으로 막힌다.
func TestPlanRejectionReleasesPreparation(t *testing.T) {
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: &stubStreamingProvider{},
		auth.StreamingProviderChzzk:   &stubStreamingProvider{},
	})
	manager.SetPlanResolver(func(context.Context, uuid.UUID) (plan.Plan, error) { return plan.Spark, nil })
	live, ownerToken, err := manager.CreateForUserWithResolution(uuid.New(), session.DefaultProvider, "", session.Resolution720p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginBroadcastPrepare(live.ID, "youtube"); err != nil {
		t.Fatal(err)
	}
	if response, _ := prepareStream(t, server.URL, live.ID, ownerToken, `{"provider":"chzzk"}`); response.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", response.StatusCode)
	}
	if got := live.BusyTargetCount(); got != 1 {
		t.Fatalf("busy targets after rejection = %d, want 1 (chzzk released)", got)
	}
}

// Spark가 두 대상을 동시에 준비해도 둘 다 플랫폼까지 가면 안 된다 — 판정이
// 선점 앞에 있으면 둘 다 "다른 대상 0"으로 통과하는 경합이 생긴다. 플랫폼 호출을
// 붙잡아 두 요청이 실제로 겹치게 한다.
func TestConcurrentPrepareCannotBypassSimulcastGate(t *testing.T) {
	for round := 0; round < 200; round++ {
		entered := make(chan struct{}, 2)
		release := make(chan struct{})
		youtube := &stubStreamingProvider{prepareErr: auth.ErrStreamingNotConnected, prepareEntered: entered, prepareRelease: release}
		chzzk := &stubStreamingProvider{prepareErr: auth.ErrStreamingNotConnected, prepareEntered: entered, prepareRelease: release}
		server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
			auth.StreamingProviderYouTube: youtube,
			auth.StreamingProviderChzzk:   chzzk,
		})
		manager.SetPlanResolver(func(context.Context, uuid.UUID) (plan.Plan, error) { return plan.Spark, nil })
		live, ownerToken, err := manager.CreateForUserWithResolution(uuid.New(), session.DefaultProvider, "", session.Resolution720p, nil)
		if err != nil {
			t.Fatal(err)
		}
		putBroadcast(t, server.URL, live.ID, ownerToken, `{"made_for_kids":false}`)
		returned := make(chan int, 2)
		var wg sync.WaitGroup
		for _, provider := range []string{"youtube", "chzzk"} {
			wg.Add(1)
			go func(provider string) {
				defer wg.Done()
				response, _ := prepareStream(t, server.URL, live.ID, ownerToken, `{"provider":"`+provider+`"}`)
				response.Body.Close()
				returned <- response.StatusCode
			}(provider)
		}
		// 두 요청이 모두 플랫폼 안에 들어갔거나(우회), 하나가 들어가고 하나가
		// 거절될 때(정상)까지 본다. 플랫폼 안의 요청은 풀어 주기 전까지 끝나지 않는다.
		inside, statuses := 0, []int{}
		timeout := time.After(5 * time.Second)
		for inside+len(statuses) < 2 {
			select {
			case <-entered:
				inside++
			case status := <-returned:
				statuses = append(statuses, status)
			case <-timeout:
				t.Fatalf("round %d: stuck (inside=%d returned=%v)", round, inside, statuses)
			}
		}
		close(release)
		wg.Wait()
		if inside > 1 {
			t.Fatalf("round %d: both targets reached the platform — simulcast gate bypassed", round)
		}
		if len(statuses) != 1 || statuses[0] != http.StatusForbidden {
			t.Fatalf("round %d: the other request returned %v, want one 403", round, statuses)
		}
		server.Close()
		manager.CloseAll()
	}
}
