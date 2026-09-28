package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"inno-live-server/internal/plan"
	"inno-live-server/internal/usage"

	"github.com/google/uuid"
)

type fakeUsageLedger struct {
	charges  map[uuid.UUID][]usage.SessionCharge
	asked    []uuid.UUID
	lastFrom time.Time
}

func (l *fakeUsageLedger) Month(_ context.Context, userID uuid.UUID, from, _, _ time.Time) ([]usage.SessionCharge, error) {
	l.asked = append(l.asked, userID)
	l.lastFrom = from
	return l.charges[userID], nil
}

func newUsageTestServer(t *testing.T, plans map[uuid.UUID]plan.Plan, ledger *fakeUsageLedger) (string, http.Header, uuid.UUID) {
	t.Helper()
	requireUser, authenticateUser, header, _, userID := testRequireUser(t)
	application, manager := newTestApplicationWithUserMiddleware(t, requireUser, authenticateUser)
	t.Cleanup(manager.CloseAll)
	if plans == nil {
		plans = map[uuid.UUID]plan.Plan{}
	}
	if _, ok := plans[userID]; !ok {
		plans[userID] = plan.Beam
	}
	application.SetPlanStore(&memoryPlanStore{plans: plans})
	application.SetUsageLedger(ledger)
	server := httptest.NewServer(application.Handler())
	t.Cleanup(server.Close)
	return server.URL, header, userID
}

func TestGetMyUsage(t *testing.T) {
	ledger := &fakeUsageLedger{charges: map[uuid.UUID][]usage.SessionCharge{}}
	baseURL, header, userID := newUsageTestServer(t, nil, ledger)
	sessionID := uuid.New()
	ledger.charges[userID] = []usage.SessionCharge{
		{SessionID: sessionID, Resolution: "fhd", Providers: []string{"chzzk", "youtube"}, OnAir: time.Hour, Charged: 3 * time.Hour},
		{SessionID: uuid.New(), Resolution: "720p", Providers: []string{"youtube"}, OnAir: 2 * time.Hour, Charged: 2 * time.Hour},
	}

	response := mustRequest(t, http.MethodGet, baseURL+"/users/me/usage?month=2026-09", nil, header)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var got usageResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	// Beam 120시간 − (3 + 2)시간 = 115시간
	if got.Month != "2026-09" || got.Plan != "beam" || got.UsedSeconds != 5*3600 ||
		got.LimitSeconds == nil || *got.LimitSeconds != 120*3600 ||
		got.RemainingSeconds == nil || *got.RemainingSeconds != 115*3600 || len(got.Broadcasts) != 2 {
		t.Fatalf("usage = %+v", got)
	}
	first := got.Broadcasts[0]
	if first.SessionID != sessionID || first.BroadcastSeconds != 3600 || first.ChargedSeconds != 3*3600 {
		t.Fatalf("broadcast = %+v", first)
	}
	if want := time.Date(2026, 9, 1, 0, 0, 0, 0, usage.LedgerLocation); !ledger.lastFrom.Equal(want) {
		t.Fatalf("ledger asked from %v, want %v (KST month start)", ledger.lastFrom, want)
	}
}

// 대상 사용자를 고르는 입력이 없다 — 인증된 본인 것만 조회되고, 미인증은 401이다.
func TestGetMyUsageIsOwnerOnly(t *testing.T) {
	ledger := &fakeUsageLedger{charges: map[uuid.UUID][]usage.SessionCharge{}}
	baseURL, header, userID := newUsageTestServer(t, nil, ledger)
	other := uuid.New()
	ledger.charges[other] = []usage.SessionCharge{{SessionID: uuid.New(), Charged: time.Hour, OnAir: time.Hour}}

	response := mustRequest(t, http.MethodGet, baseURL+"/users/me/usage?user_id="+other.String(), nil, nil)
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", response.StatusCode)
	}

	response = mustRequest(t, http.MethodGet, baseURL+"/users/me/usage?user_id="+other.String(), nil, header)
	var got usageResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(ledger.asked) != 1 || ledger.asked[0] != userID || got.UsedSeconds != 0 || len(got.Broadcasts) != 0 {
		t.Fatalf("asked=%v usage=%+v, want only the caller's own (empty) usage", ledger.asked, got)
	}
}

func TestGetMyUsageRejectsBadMonthAndHandlesUnlimited(t *testing.T) {
	ledger := &fakeUsageLedger{charges: map[uuid.UUID][]usage.SessionCharge{}}
	requireUserPlans := map[uuid.UUID]plan.Plan{}
	baseURL, header, userID := newUsageTestServer(t, requireUserPlans, ledger)

	response := mustRequest(t, http.MethodGet, baseURL+"/users/me/usage?month=2026-13", nil, header)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad month status = %d, want 400", response.StatusCode)
	}

	requireUserPlans[userID] = plan.Glow
	response = mustRequest(t, http.MethodGet, baseURL+"/users/me/usage", nil, header)
	var got usageResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if got.LimitSeconds != nil || got.RemainingSeconds != nil {
		t.Fatalf("glow usage = %+v, want unlimited (null limit)", got)
	}
}

// 남은 방송 시간을 방식별 배수로 나눠 주고, 플랜이 허용하지 않는 방식도 잠금으로 함께 준다(#276).
func TestGetMyUsageAvailableByMode(t *testing.T) {
	cases := []struct {
		plan    plan.Plan
		used    time.Duration
		seconds map[plan.Mode]int64 // -1 = null(무제한)
		allowed map[plan.Mode]bool
		perOnce int64 // -1 = null
	}{
		// BM 예시: 남은 방송 시간 42시간 → 720p 42 · FHD 21 · 720p 동시 21 · FHD 동시 14
		{plan.Plasma, (240 - 42) * time.Hour,
			map[plan.Mode]int64{plan.Mode720pSingle: 42 * 3600, plan.ModeFHDSingle: 21 * 3600, plan.Mode720pMulti: 21 * 3600, plan.ModeFHDMulti: 14 * 3600},
			map[plan.Mode]bool{plan.Mode720pSingle: true, plan.ModeFHDSingle: true, plan.Mode720pMulti: true, plan.ModeFHDMulti: true}, 12 * 3600},
		{plan.Beam, 115 * time.Hour,
			map[plan.Mode]int64{plan.Mode720pSingle: 5 * 3600, plan.ModeFHDSingle: 9000, plan.Mode720pMulti: 9000, plan.ModeFHDMulti: 6000},
			map[plan.Mode]bool{plan.Mode720pSingle: true, plan.ModeFHDSingle: true, plan.Mode720pMulti: true, plan.ModeFHDMulti: false}, 8 * 3600},
		// 다 쓰면 모두 0이다(음수가 아니다).
		{plan.Spark, 6 * time.Hour,
			map[plan.Mode]int64{plan.Mode720pSingle: 0, plan.ModeFHDSingle: 0, plan.Mode720pMulti: 0, plan.ModeFHDMulti: 0},
			map[plan.Mode]bool{plan.Mode720pSingle: true}, 2 * 3600},
		{plan.Glow, 10 * time.Hour,
			map[plan.Mode]int64{plan.Mode720pSingle: -1, plan.ModeFHDSingle: -1, plan.Mode720pMulti: -1, plan.ModeFHDMulti: -1},
			map[plan.Mode]bool{}, -1},
	}
	for _, test := range cases {
		t.Run(string(test.plan), func(t *testing.T) {
			ledger := &fakeUsageLedger{charges: map[uuid.UUID][]usage.SessionCharge{}}
			plans := map[uuid.UUID]plan.Plan{}
			baseURL, header, userID := newUsageTestServer(t, plans, ledger)
			plans[userID] = test.plan
			ledger.charges[userID] = []usage.SessionCharge{{SessionID: uuid.New(), Charged: test.used, OnAir: test.used}}

			response := mustRequest(t, http.MethodGet, baseURL+"/users/me/usage", nil, header)
			var got usageResponse
			if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if len(got.AvailableByMode) != 4 {
				t.Fatalf("available_by_mode = %+v, want all 4 modes", got.AvailableByMode)
			}
			for _, entry := range got.AvailableByMode {
				want := test.seconds[entry.Mode]
				if want < 0 && entry.Seconds != nil || want >= 0 && (entry.Seconds == nil || *entry.Seconds != want) {
					t.Fatalf("%s seconds = %v, want %d", entry.Mode, entry.Seconds, want)
				}
				if entry.Allowed != test.allowed[entry.Mode] || entry.Multiplier != entry.Mode.Units() {
					t.Fatalf("%s = %+v, want allowed %v", entry.Mode, entry, test.allowed[entry.Mode])
				}
			}
			if test.perOnce < 0 && got.MaxPerBroadcastSeconds != nil ||
				test.perOnce >= 0 && (got.MaxPerBroadcastSeconds == nil || *got.MaxPerBroadcastSeconds != test.perOnce) {
				t.Fatalf("max_per_broadcast_seconds = %v, want %d", got.MaxPerBroadcastSeconds, test.perOnce)
			}
		})
	}
}
