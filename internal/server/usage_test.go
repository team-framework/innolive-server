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
