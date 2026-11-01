package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"inno-live-server/internal/plan"
	"inno-live-server/internal/usage"

	"github.com/google/uuid"
)

type memoryLocalTakes struct {
	calls [][]usage.LocalTake
}

func (m *memoryLocalTakes) RecordLocalTakes(_ context.Context, takes []usage.LocalTake) error {
	m.calls = append(m.calls, takes)
	return nil
}

func newLocalTakesTestServer(t *testing.T, recorder *memoryLocalTakes) (string, http.Header, uuid.UUID) {
	t.Helper()
	requireUser, authenticateUser, header, _, userID := testRequireUser(t)
	application, manager := newTestApplicationWithUserMiddleware(t, requireUser, authenticateUser)
	t.Cleanup(manager.CloseAll)
	application.SetLocalTakeRecorder(recorder)
	server := httptest.NewServer(application.Handler())
	t.Cleanup(server.Close)
	return server.URL, header, userID
}

func localTakeJSON(takeID, startedAt, endedAt string, seconds int) string {
	return fmt.Sprintf(`{"take_id":%q,"device_id":"device-1","started_at":%q,"ended_at":%q,"duration_seconds":%d}`, takeID, startedAt, endedAt, seconds)
}

func TestPostLocalTakesStoresCallerTakesOnce(t *testing.T) {
	recorder := &memoryLocalTakes{}
	baseURL, header, userID := newLocalTakesTestServer(t, recorder)
	first, second := uuid.New(), uuid.New()
	body := `{"takes":[` +
		localTakeJSON(first.String(), "2026-10-31T14:50:00Z", "2026-10-31T15:00:00Z", 600) + `,` +
		localTakeJSON(second.String(), "2026-11-01T01:00:00+09:00", "2026-11-01T01:00:30.4+09:00", 31) + `,` +
		localTakeJSON(first.String(), "2026-10-31T14:50:00Z", "2026-10-31T15:00:00Z", 600) + `]}`

	response := mustRequest(t, http.MethodPost, baseURL+"/users/me/usage/local-takes", strings.NewReader(body), header)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	var got localTakesResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.TakeIDs) != 2 || got.TakeIDs[0] != first || got.TakeIDs[1] != second {
		t.Fatalf("acknowledged = %v, want [%s %s]", got.TakeIDs, first, second)
	}
	if len(recorder.calls) != 1 || len(recorder.calls[0]) != 2 {
		t.Fatalf("recorded = %+v, want one call with two takes", recorder.calls)
	}
	stored := recorder.calls[0][1]
	if stored.UserID == nil || *stored.UserID != userID || stored.DeviceID != "device-1" || stored.DurationSeconds != 31 ||
		!stored.StartedAt.Equal(time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("stored take = %+v, want the caller's take in UTC", stored)
	}
}

func TestPostLocalTakesRejectsInvalidTakes(t *testing.T) {
	recorder := &memoryLocalTakes{}
	baseURL, header, _ := newLocalTakesTestServer(t, recorder)
	id := uuid.NewString()
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"빈 목록", `{"takes":[]}`, "takes"},
		{"take_id 형식", `{"takes":[` + localTakeJSON("take-1", "2026-11-01T00:00:00Z", "2026-11-01T00:01:00Z", 60) + `]}`, "takes[0].take_id"},
		{"끝이 시작보다 앞", `{"takes":[` + localTakeJSON(id, "2026-11-01T00:01:00Z", "2026-11-01T00:00:00Z", 60) + `]}`, "takes[0].ended_at"},
		{"구간보다 긴 시간", `{"takes":[` + localTakeJSON(id, "2026-11-01T00:00:00Z", "2026-11-01T00:01:00Z", 62) + `]}`, "takes[0].duration_seconds"},
		{"0초", `{"takes":[` + localTakeJSON(id, "2026-11-01T00:00:00Z", "2026-11-01T00:01:00Z", 0) + `]}`, "takes[0].duration_seconds"},
		{"기기 ID 없음", `{"takes":[{"take_id":"` + id + `","device_id":" ","started_at":"2026-11-01T00:00:00Z","ended_at":"2026-11-01T00:01:00Z","duration_seconds":60}]}`, "takes[0].device_id"},
	}
	for _, test := range cases {
		response := mustRequest(t, http.MethodPost, baseURL+"/users/me/usage/local-takes", strings.NewReader(test.body), header)
		var got struct {
			Error struct {
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		_ = json.NewDecoder(response.Body).Decode(&got)
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest || got.Error.Details["field"] != test.field {
			t.Fatalf("%s: status=%d field=%v, want 400 %s", test.name, response.StatusCode, got.Error.Details["field"], test.field)
		}
	}
	response := mustRequest(t, http.MethodPost, baseURL+"/users/me/usage/local-takes", strings.NewReader(`{"takes":[`+localTakeJSON(id, "2026-11-01T00:00:00Z", "2026-11-01T00:01:00Z", 60)+`]}`), nil)
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", response.StatusCode)
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("recorded %d calls for rejected requests", len(recorder.calls))
	}
}

func TestGetMyUsageCountsLocalTakesAsDeviceTime(t *testing.T) {
	ledger := &fakeUsageLedger{charges: map[uuid.UUID][]usage.SessionCharge{}}
	baseURL, header, userID := newUsageTestServer(t, map[uuid.UUID]plan.Plan{}, ledger)
	ledger.charges[userID] = []usage.SessionCharge{
		{SessionID: uuid.New(), Resolution: "720p", Providers: []string{"youtube"}, OnDevice: true, OnAir: time.Hour, Charged: time.Hour},
		{SessionID: uuid.New(), Providers: []string{}, OnDevice: true, Local: true, OnAir: 30 * time.Minute, Charged: 30 * time.Minute},
	}

	response := mustRequest(t, http.MethodGet, baseURL+"/users/me/usage", nil, header)
	defer response.Body.Close()
	var got usageResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	// Beam 온디바이스 120시간 − 1.5시간
	if got.DeviceUsedSeconds != 5400 || got.DeviceLocalSeconds != 1800 || got.UsedSeconds != 0 ||
		got.DeviceRemainingSeconds == nil || *got.DeviceRemainingSeconds != 120*3600-5400 || len(got.Broadcasts) != 1 {
		t.Fatalf("usage = %+v, want local 30m inside device used and out of broadcasts", got)
	}
}
