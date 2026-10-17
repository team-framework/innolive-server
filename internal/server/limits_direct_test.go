package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/plan"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
)

type directLimitFixture struct {
	app        *Server
	baseURL    string
	live       *session.Session
	ownerToken string
	youtube    *stubStreamingProvider
	chzzk      *stubStreamingProvider
	ledger     *sessionOnAirLedger
}

// newDirectLimitFixture는 유튜브·치지직을 직접 송출로 동시에 라이브로 올린 Beam
// 온디바이스 세션이다. 원장의 방송 시간은 라이브 뒤에 테스트가 정한다.
func newDirectLimitFixture(t *testing.T) directLimitFixture {
	t.Helper()
	youtube := &stubStreamingProvider{prepared: streaming.PreparedBroadcast{
		Provider: auth.StreamingProviderYouTube, BroadcastID: "b1", IngestURL: "rtmps://a.rtmp.youtube.com/live2/secret-key",
	}}
	chzzk := &stubStreamingProvider{prepared: chzzkPrepared()}
	server, manager, app := newStreamTestApplicationWithServer(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: youtube,
		auth.StreamingProviderChzzk:   chzzk,
	})
	app.SetHeartbeatStore(&fakeHeartbeatStore{})
	app.cfg.BroadcastIdleTimeout = 5 * time.Minute
	manager.SetPlanResolver(func(context.Context, uuid.UUID) (plan.Plan, error) { return plan.Beam, nil })
	live, ownerToken, err := manager.CreateForUserWithResolution(uuid.New(), session.DefaultProvider, session.AIProcessingOnDevice, session.Resolution720p, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &sessionOnAirLedger{sessionID: uuid.MustParse(live.ID)}
	app.usageLedger = ledger
	putBroadcast(t, server.URL, live.ID, ownerToken, `{"made_for_kids":false}`)
	for _, body := range []string{`{"provider":"youtube"}`, `{"provider":"chzzk"}`} {
		if response, payload := prepareStream(t, server.URL, live.ID, ownerToken, body); response.StatusCode != http.StatusOK {
			t.Fatalf("prepare %s = %d (payload %v)", body, response.StatusCode, payload)
		}
	}
	if response, payload := goLive(t, server.URL, live.ID, ownerToken); response.StatusCode != http.StatusOK {
		t.Fatalf("go live = %d (payload %v)", response.StatusCode, payload)
	}
	if got := len(live.DirectLiveTargets()); got != 2 {
		t.Fatalf("direct live targets = %d, want 2", got)
	}
	return directLimitFixture{app: app, baseURL: server.URL, live: live, ownerToken: ownerToken, youtube: youtube, chzzk: chzzk, ledger: ledger}
}

func stopTargets(t *testing.T, payload map[string]any) []string {
	t.Helper()
	raw, ok := payload["stop_targets"].([]any)
	if !ok {
		t.Fatalf("stop_targets = %v, want an array", payload["stop_targets"])
	}
	stops := make([]string, len(raw))
	for i, value := range raw {
		stops[i], _ = value.(string)
	}
	return stops
}

// TestDirectBroadcastStoppedAtBroadcastLimit: 1회 최대에 닿은 직접 송출은 egress가
// 없어도 집행 대상이다. 유튜브는 플랫폼에서 끝내고, 두 대상 모두 다음 하트비트로
// 앱에 끊을 대상을 알린다 — 라이브 대상이 없어도 그 응답은 거절하지 않는다.
func TestDirectBroadcastStoppedAtBroadcastLimit(t *testing.T) {
	fixture := newDirectLimitFixture(t)
	fixture.ledger.onAir = 8 * time.Hour // Beam 1회 최대

	fixture.app.enforceLimits(context.Background(), time.Now())

	if got := fixture.live.DirectLiveTargets(); len(got) != 0 {
		t.Fatalf("direct live targets = %v, want none after the limit", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if calls, _ := fixture.youtube.ended(); calls == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls, _ := fixture.youtube.ended(); calls != 1 {
		t.Fatalf("youtube end live calls = %d, want the platform broadcast ended", calls)
	}
	response, payload := postHeartbeat(t, fixture.baseURL, fixture.live.ID, fixture.ownerToken, validHeartbeat)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat after limit = %d (payload %v)", response.StatusCode, payload)
	}
	if stops := stopTargets(t, payload); len(stops) != 2 || stops[0] != "chzzk" || stops[1] != "youtube" {
		t.Fatalf("stop_targets = %v, want [chzzk youtube]", stops)
	}
	// 한 번 알린 대상은 다시 싣지 않는다.
	if response, payload := postHeartbeat(t, fixture.baseURL, fixture.live.ID, fixture.ownerToken, validHeartbeat); response.StatusCode != http.StatusConflict {
		t.Fatalf("second heartbeat = %d (payload %v), want 409", response.StatusCode, payload)
	}
}

// TestDirectBroadcastStoppedWithoutHeartbeat: 연결된 대상이 있는 하트비트가 입력
// 없음 시간만큼 없으면 끝낸다. 연결이 끊긴 보고는 시계를 열지 않는다.
func TestDirectBroadcastStoppedWithoutHeartbeat(t *testing.T) {
	fixture := newDirectLimitFixture(t)
	before := fixture.live.HeartbeatIdleSince()
	disconnected := `{"sent_at":"2026-10-12T08:30:00Z","targets":[{"provider":"youtube","connected":false}],"video":{"fps":0,"dropped_frames":0},"anonymization":{"failed_frames":0},"platform":"ios","model":"x"}`
	if response, payload := postHeartbeat(t, fixture.baseURL, fixture.live.ID, fixture.ownerToken, disconnected); response.StatusCode != http.StatusOK {
		t.Fatalf("disconnected heartbeat = %d (payload %v)", response.StatusCode, payload)
	}
	if !fixture.live.HeartbeatIdleSince().Equal(before) {
		t.Fatal("disconnected heartbeat restarted the idle clock")
	}

	fixture.app.enforceLimits(context.Background(), before.Add(6*time.Minute))

	if got := fixture.live.DirectLiveTargets(); len(got) != 0 {
		t.Fatalf("direct live targets = %v, want none after idle timeout", got)
	}
}

// TestDirectBroadcastKeptAliveByHeartbeat: 하트비트가 이어지면 라이브 전환 뒤
// 입력 없음 시간이 지나도 끝내지 않는다.
func TestDirectBroadcastKeptAliveByHeartbeat(t *testing.T) {
	fixture := newDirectLimitFixture(t)
	start := fixture.live.HeartbeatIdleSince()
	fixture.live.MarkHeartbeat(start.Add(3 * time.Minute))

	fixture.app.enforceLimits(context.Background(), start.Add(6*time.Minute))

	if got := fixture.live.DirectLiveTargets(); len(got) != 2 {
		t.Fatalf("direct live targets = %v, want both kept live", got)
	}
}

// TestDirectBroadcastRejectsResolutionChange: 직접 송출 라이브 중 해상도 변경은
// busy가 아니라 on_device_processing으로 거절한다.
func TestDirectBroadcastRejectsResolutionChange(t *testing.T) {
	fixture := newDirectLimitFixture(t)
	request, err := http.NewRequest(http.MethodPut, fixture.baseURL+"/sessions/"+fixture.live.ID+"/broadcast-resolution", strings.NewReader(`{"resolution":"fhd"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Session-Owner-Token", fixture.ownerToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&payload)
	if response.StatusCode != http.StatusConflict || streamErrorCode(payload) != "on_device_processing" {
		t.Fatalf("resolution change = %d %q, want 409 on_device_processing", response.StatusCode, streamErrorCode(payload))
	}
}
