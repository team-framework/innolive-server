package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/plan"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
	pionmedia "github.com/pion/webrtc/v4/pkg/media"
)

// liveSwitchFixture는 실제 영상 트랙이 붙은 세션에서 유튜브·치지직을 라이브로
// 세운다. 송출 주소는 임시 파일이라 egress가 실제로 송출 단계까지 간다(일시정지 가능).
type liveSwitchFixture struct {
	baseURL    string
	sessionID  string
	ownerToken string
	youtube    *stubStreamingProvider
	chzzk      *stubStreamingProvider
	manager    *session.Manager
}

func newLiveSwitchFixture(t *testing.T, chzzkWait time.Duration) liveSwitchFixture {
	t.Helper()
	return newModeSwitchFixture(t, chzzkWait, "", session.Resolution720p, "youtube", "chzzk")
}

// newModeSwitchFixture는 플랜·시작 해상도·시작 대상을 골라 라이브를 세운다.
func newModeSwitchFixture(t *testing.T, chzzkWait time.Duration, owner plan.Plan, resolution string, targets ...string) liveSwitchFixture {
	t.Helper()
	return newConfiguredModeSwitchFixture(t, chzzkWait, owner, resolution, nil, targets...)
}

// newConfiguredModeSwitchFixture는 준비 전에 플랫폼 대역을 고칠 수 있다.
func newConfiguredModeSwitchFixture(t *testing.T, chzzkWait time.Duration, owner plan.Plan, resolution string, configure func(youtube, chzzk *stubStreamingProvider), targets ...string) liveSwitchFixture {
	t.Helper()
	previousWait, previousRetry := resolutionSwitchChzzkWait, resolutionSwitchRetryInterval
	resolutionSwitchChzzkWait, resolutionSwitchRetryInterval = chzzkWait, 20*time.Millisecond
	t.Cleanup(func() { resolutionSwitchChzzkWait, resolutionSwitchRetryInterval = previousWait, previousRetry })

	output := t.TempDir()
	youtube := &stubStreamingProvider{prepared: streaming.PreparedBroadcast{Provider: auth.StreamingProviderYouTube, BroadcastID: "yt-1", IngestURL: filepath.Join(output, "youtube.flv")}}
	chzzk := &stubStreamingProvider{prepared: streaming.PreparedBroadcast{Provider: auth.StreamingProviderChzzk, IngestURL: filepath.Join(output, "chzzk.flv")}}
	if configure != nil {
		configure(youtube, chzzk)
	}
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: youtube,
		auth.StreamingProviderChzzk:   chzzk,
	})
	manager.SetPlanResolver(func(context.Context, uuid.UUID) (plan.Plan, error) { return owner, nil })
	live, ownerToken, err := manager.CreateForUserWithResolution(uuid.New(), session.DefaultProvider, "", resolution, nil)
	if err != nil {
		t.Fatal(err)
	}
	created := struct{ SessionID string }{live.ID}
	track, _ := connectTestPublisher(t, server.URL, created.SessionID, ownerToken)
	frames := generateVP8Frames(t, 30)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		ticker := time.NewTicker(time.Second / 30)
		defer ticker.Stop()
		for index := 0; ; index++ {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := track.WriteSample(pionmedia.Sample{Data: frames[index%len(frames)], Duration: time.Second / 30}); err != nil {
					return
				}
			}
		}
	}()
	// 트랙이 파이프라인에 붙어야 유튜브 준비(egress 부착)가 된다.
	deadline := time.Now().Add(10 * time.Second)
	for {
		media, _ := getSessionPayload(t, server.URL, created.SessionID, ownerToken)["media"].(map[string]any)
		if media["raw_video_track"] != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("video track never reached the session")
		}
		time.Sleep(20 * time.Millisecond)
	}
	putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`)
	for _, target := range targets {
		body := `{"provider":"` + target + `"}`
		if response, payload := prepareStream(t, server.URL, created.SessionID, ownerToken, body); response.StatusCode != http.StatusOK {
			t.Fatalf("prepare %s = %d %v", body, response.StatusCode, payload)
		}
	}
	if response, payload := goLive(t, server.URL, created.SessionID, ownerToken); response.StatusCode != http.StatusOK {
		t.Fatalf("go live = %d %v", response.StatusCode, payload)
	}
	return liveSwitchFixture{baseURL: server.URL, sessionID: created.SessionID, ownerToken: ownerToken, youtube: youtube, chzzk: chzzk, manager: manager}
}

func (f liveSwitchFixture) putResolution(t *testing.T, resolution string) (int, map[string]any) {
	t.Helper()
	return f.put(t, "broadcast-resolution", `{"resolution":"`+resolution+`"}`)
}

func (f liveSwitchFixture) putMode(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	return f.put(t, "broadcast-mode", body)
}

func (f liveSwitchFixture) put(t *testing.T, path, body string) (int, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut, f.baseURL+"/sessions/"+f.sessionID+"/"+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Session-Owner-Token", f.ownerToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&payload)
	return response.StatusCode, payload
}

// waitSwitch는 전환이 끝날 때까지 세션을 조회해 마지막 응답을 돌려준다.
func (f liveSwitchFixture) waitSwitch(t *testing.T) (map[string]any, map[string]any) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		payload := getSessionPayload(t, f.baseURL, f.sessionID, f.ownerToken)
		state, _ := payload["resolution_switch"].(map[string]any)
		if state != nil && state["status"] != "switching" {
			return payload, state
		}
		if time.Now().After(deadline) {
			t.Fatalf("resolution switch did not finish: %v", state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (p *stubStreamingProvider) calls() (prepare, goLive, endLive int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prepareCalls, p.goLiveCalls, p.endLiveCalls
}

// 방송 중 해상도 변경은 두 대상을 끝내고 새 해상도로 새 방송을 연다(#283).
func TestResolutionSwitchReopensEveryLiveTarget(t *testing.T) {
	fixture := newLiveSwitchFixture(t, 50*time.Millisecond)

	status, payload := fixture.putResolution(t, "fhd")
	if status != http.StatusAccepted {
		t.Fatalf("status = %d %v, want 202", status, payload)
	}
	if state, _ := payload["resolution_switch"].(map[string]any); state["status"] != "switching" {
		t.Fatalf("resolution_switch = %v, want switching", state)
	}
	final, state := fixture.waitSwitch(t)
	if state["status"] != "done" || state["failed_targets"] != nil || final["broadcast_resolution"] != "fhd" {
		t.Fatalf("final = %v / resolution %v, want done fhd", state, final["broadcast_resolution"])
	}
	for _, provider := range []string{"youtube", "chzzk"} {
		if phase := targetBroadcastPhase(t, final, provider); phase != "live" {
			t.Fatalf("%s phase = %q, want live", provider, phase)
		}
	}
	// 대상마다 준비·전환이 한 번씩 더 불렸고, 라이브였던 유튜브 방송은 끝냈다.
	if prepare, goLive, endLive := fixture.youtube.calls(); prepare != 2 || goLive != 2 || endLive != 1 {
		t.Fatalf("youtube calls prepare=%d goLive=%d endLive=%d, want 2/2/1", prepare, goLive, endLive)
	}
	if prepare, goLive, _ := fixture.chzzk.calls(); prepare != 2 || goLive != 2 {
		t.Fatalf("chzzk calls prepare=%d goLive=%d, want 2/2", prepare, goLive)
	}
}

// 한 대상의 새 방송이 실패해도 나머지는 라이브로 남고, 실패한 대상은 치운다.
func TestResolutionSwitchReportsFailedTarget(t *testing.T) {
	fixture := newLiveSwitchFixture(t, 50*time.Millisecond)
	fixture.chzzk.mu.Lock()
	fixture.chzzk.goLiveErr = auth.ErrStreamingReconnectRequired
	fixture.chzzk.mu.Unlock()

	if status, payload := fixture.putResolution(t, "fhd"); status != http.StatusAccepted {
		t.Fatalf("status = %d %v", status, payload)
	}
	final, state := fixture.waitSwitch(t)
	failed, _ := state["failed_targets"].([]any)
	if state["status"] != "done" || len(failed) != 1 || failed[0].(map[string]any)["provider"] != "chzzk" ||
		failed[0].(map[string]any)["code"] != "streaming_reconnect_required" {
		t.Fatalf("state = %v, want done with chzzk failed", state)
	}
	if phase := targetBroadcastPhase(t, final, "youtube"); phase != "live" {
		t.Fatalf("youtube phase = %q, want live", phase)
	}
	if phase := targetBroadcastPhase(t, final, "chzzk"); phase != "idle" {
		t.Fatalf("chzzk phase = %q, want idle (failed broadcast released)", phase)
	}
}

// 전환 중 방송 종료를 누르면 아직 열지 않은 대상은 다시 열지 않는다.
func TestResolutionSwitchStopsWhenUserStops(t *testing.T) {
	fixture := newLiveSwitchFixture(t, 3*time.Second)
	if status, payload := fixture.putResolution(t, "fhd"); status != http.StatusAccepted {
		t.Fatalf("status = %d %v", status, payload)
	}
	// 유튜브는 곧바로 다시 열리고, 치지직은 대기 중이다.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, goLive, _ := fixture.youtube.calls(); goLive == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("youtube was not reopened")
		}
		time.Sleep(20 * time.Millisecond)
	}
	postStream(t, fixture.baseURL, fixture.sessionID, fixture.ownerToken, "stop", "")
	_, state := fixture.waitSwitch(t)
	if state["status"] != "canceled" {
		t.Fatalf("state = %v, want canceled", state)
	}
	time.Sleep(3500 * time.Millisecond)
	if prepare, _, _ := fixture.chzzk.calls(); prepare != 1 {
		t.Fatalf("chzzk prepare calls = %d, want 1 (not reopened after stop)", prepare)
	}
}

// 준비만 된 대상이 있으면 끝낼 수도 이어 갈 수도 없어 거절한다.
func TestResolutionChangeRejectsPreparedTarget(t *testing.T) {
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderChzzk: &stubStreamingProvider{},
	})
	created, ownerToken := createTestSession(t, server.URL, nil)
	markPrepared(t, manager, created.SessionID, auth.StreamingProviderChzzk, streaming.ChzzkIngestURL+"/key")
	fixture := liveSwitchFixture{baseURL: server.URL, sessionID: created.SessionID, ownerToken: ownerToken}
	if status, payload := fixture.putResolution(t, "fhd"); status != http.StatusConflict || streamErrorCode(payload) != "broadcast_busy" {
		t.Fatalf("status = %d %q, want 409 broadcast_busy", status, streamErrorCode(payload))
	}
}

func targetStreamStatus(payload map[string]any, provider string) string {
	targets, _ := payload["targets"].([]any)
	for _, entry := range targets {
		target, _ := entry.(map[string]any)
		if target["provider"] == provider {
			stream, _ := target["stream"].(map[string]any)
			status, _ := stream["status"].(string)
			return status
		}
	}
	return ""
}

// 멈춰 있던 대상은 새 방송을 연 뒤 다시 멈춘다. 멈추지 않았던 대상은 그대로 송출한다.
func TestResolutionSwitchKeepsPausedTargetPaused(t *testing.T) {
	fixture := newLiveSwitchFixture(t, 50*time.Millisecond)
	// egress가 첫 프레임을 받아 송출 단계에 들어가야 멈출 수 있다.
	deadline := time.Now().Add(15 * time.Second)
	for {
		response, _ := postStream(t, fixture.baseURL, fixture.sessionID, fixture.ownerToken, "pause?provider=youtube", "")
		if response.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not pause youtube before the switch: %d", response.StatusCode)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if status, payload := fixture.putResolution(t, "fhd"); status != http.StatusAccepted {
		t.Fatalf("status = %d %v", status, payload)
	}
	if _, state := fixture.waitSwitch(t); state["status"] != "done" {
		t.Fatalf("state = %v, want done", state)
	}
	deadline = time.Now().Add(20 * time.Second)
	for {
		payload := getSessionPayload(t, fixture.baseURL, fixture.sessionID, fixture.ownerToken)
		youtube, chzzk := targetStreamStatus(payload, "youtube"), targetStreamStatus(payload, "chzzk")
		if youtube == "paused" {
			if chzzk == "paused" {
				t.Fatal("chzzk was not paused before the switch and must keep streaming")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("youtube stream = %q after the switch, want paused again", youtube)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 모든 대상이 멈춘 상태에서 대상을 더하면 새 대상도 멈춘 채로 연다(#320).
func TestBroadcastModeAddsTargetPausedWhenAllPaused(t *testing.T) {
	fixture := newModeSwitchFixture(t, time.Second, plan.Beam, session.Resolution720p, "youtube")
	deadline := time.Now().Add(15 * time.Second)
	for {
		response, _ := postStream(t, fixture.baseURL, fixture.sessionID, fixture.ownerToken, "pause?provider=youtube", "")
		if response.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not pause youtube before the switch: %d", response.StatusCode)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if status, payload := fixture.putMode(t, `{"targets":["youtube","chzzk"]}`); status != http.StatusAccepted {
		t.Fatalf("status = %d %v", status, payload)
	}
	if _, state := fixture.waitSwitch(t); state["status"] != "done" {
		t.Fatalf("state = %v, want done", state)
	}
	deadline = time.Now().Add(20 * time.Second)
	for {
		payload := getSessionPayload(t, fixture.baseURL, fixture.sessionID, fixture.ownerToken)
		youtube, chzzk := targetStreamStatus(payload, "youtube"), targetStreamStatus(payload, "chzzk")
		if youtube == "paused" && chzzk == "paused" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("youtube=%q chzzk=%q after the switch, want both paused", youtube, chzzk)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestPausedAfterSwitch(t *testing.T) {
	for _, test := range []struct {
		name             string
		live, paused, to []string
		want             []string
	}{
		{"all paused adds new target", []string{"youtube"}, []string{"youtube"}, []string{"chzzk", "youtube"}, []string{"chzzk", "youtube"}},
		{"some paused keeps only those", []string{"chzzk", "youtube"}, []string{"youtube"}, []string{"chzzk", "youtube"}, []string{"youtube"}},
		{"none paused", []string{"youtube"}, nil, []string{"chzzk", "youtube"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := pausedAfterSwitch(test.live, test.paused, test.to); !slices.Equal(got, test.want) {
				t.Fatalf("pausedAfterSwitch() = %v, want %v", got, test.want)
			}
		})
	}
}

func (p *stubStreamingProvider) prepareCount() int {
	prepare, _, _ := p.calls()
	return prepare
}

// 해상도가 같으면 계속되는 대상은 끊지 않고 추가되는 대상만 연다(720p 단독 → 720p 동시).
func TestBroadcastModeAddsTargetWithoutTouchingLiveOne(t *testing.T) {
	fixture := newModeSwitchFixture(t, time.Second, plan.Beam, session.Resolution720p, "youtube")
	if status, payload := fixture.putMode(t, `{"targets":["youtube","chzzk"]}`); status != http.StatusAccepted {
		t.Fatalf("status = %d %v", status, payload)
	}
	final, state := fixture.waitSwitch(t)
	if state["status"] != "done" || state["failed_targets"] != nil {
		t.Fatalf("state = %v, want done", state)
	}
	for _, provider := range []string{"youtube", "chzzk"} {
		if phase := targetBroadcastPhase(t, final, provider); phase != "live" {
			t.Fatalf("%s phase = %q, want live", provider, phase)
		}
	}
	if prepare, goLive, endLive := fixture.youtube.calls(); prepare != 1 || goLive != 1 || endLive != 0 {
		t.Fatalf("youtube calls prepare=%d goLive=%d endLive=%d, want untouched 1/1/0", prepare, goLive, endLive)
	}
	if prepare, goLive, _ := fixture.chzzk.calls(); prepare != 1 || goLive != 1 {
		t.Fatalf("chzzk calls prepare=%d goLive=%d, want opened once", prepare, goLive)
	}
}

// 해상도를 바꿔 새 방송을 열어도 유튜브는 준비 때 고른 채널로 다시 연다(#390).
func TestResolutionSwitchReopensYouTubeOnPinnedChannel(t *testing.T) {
	pinned := uuid.New()
	fixture := newConfiguredModeSwitchFixture(t, 50*time.Millisecond, plan.Plasma, session.Resolution720p, func(youtube, _ *stubStreamingProvider) {
		youtube.prepared.AccountID = pinned
	}, "youtube")
	if status, payload := fixture.putResolution(t, "fhd"); status != http.StatusAccepted {
		t.Fatalf("status = %d %v", status, payload)
	}
	if _, state := fixture.waitSwitch(t); state["status"] != "done" || state["failed_targets"] != nil {
		t.Fatalf("state = %v, want done", state)
	}
	fixture.youtube.mu.Lock()
	accounts := slices.Clone(fixture.youtube.preparedAccounts)
	lastGoLive := fixture.youtube.lastGoLive
	fixture.youtube.mu.Unlock()
	if len(accounts) != 2 || accounts[1] != pinned {
		t.Fatalf("prepare accounts = %v, want reopen on %v", accounts, pinned)
	}
	if lastGoLive.AccountID != pinned {
		t.Fatalf("go live account = %v, want %v", lastGoLive.AccountID, pinned)
	}
	final := getSessionPayload(t, fixture.baseURL, fixture.sessionID, fixture.ownerToken)
	targets, _ := final["targets"].([]any)
	if len(targets) != 1 || targets[0].(map[string]any)["account_id"] != pinned.String() {
		t.Fatalf("targets = %v, want pinned account in response", targets)
	}
}

// 빠지는 대상만 끝낸다(720p 동시 → 720p 단독).
func TestBroadcastModeRemovesTargetOnly(t *testing.T) {
	fixture := newModeSwitchFixture(t, time.Second, plan.Beam, session.Resolution720p, "youtube", "chzzk")
	if status, payload := fixture.putMode(t, `{"targets":["youtube"]}`); status != http.StatusAccepted {
		t.Fatalf("status = %d %v", status, payload)
	}
	final, state := fixture.waitSwitch(t)
	if state["status"] != "done" {
		t.Fatalf("state = %v, want done", state)
	}
	if phase := targetBroadcastPhase(t, final, "youtube"); phase != "live" {
		t.Fatalf("youtube phase = %q, want live", phase)
	}
	if phase := targetBroadcastPhase(t, final, "chzzk"); phase != "idle" {
		t.Fatalf("chzzk phase = %q, want idle", phase)
	}
	if prepare, _, endLive := fixture.youtube.calls(); prepare != 1 || endLive != 0 {
		t.Fatalf("youtube prepare=%d endLive=%d, want untouched", prepare, endLive)
	}
	if fixture.chzzk.prepareCount() != 1 {
		t.Fatalf("chzzk prepare calls = %d, want not reopened", fixture.chzzk.prepareCount())
	}
}

// 해상도와 대상 구성을 함께 바꾼다(Beam FHD 단독 → 720p 동시).
func TestBroadcastModeChangesResolutionAndTargets(t *testing.T) {
	fixture := newModeSwitchFixture(t, time.Second, plan.Beam, session.ResolutionFHD, "youtube")
	if status, payload := fixture.putMode(t, `{"resolution":"720p","targets":["youtube","chzzk"]}`); status != http.StatusAccepted {
		t.Fatalf("status = %d %v", status, payload)
	}
	final, state := fixture.waitSwitch(t)
	if state["status"] != "done" || state["failed_targets"] != nil || final["broadcast_resolution"] != "720p" {
		t.Fatalf("state = %v resolution = %v, want done 720p", state, final["broadcast_resolution"])
	}
	for _, provider := range []string{"youtube", "chzzk"} {
		if phase := targetBroadcastPhase(t, final, provider); phase != "live" {
			t.Fatalf("%s phase = %q, want live", provider, phase)
		}
	}
	// 유튜브는 새 해상도로 다시 열렸고(이전 방송 종료), 치지직은 처음 열렸다.
	if prepare, goLive, endLive := fixture.youtube.calls(); prepare != 2 || goLive != 2 || endLive != 1 {
		t.Fatalf("youtube calls prepare=%d goLive=%d endLive=%d, want 2/2/1", prepare, goLive, endLive)
	}
	if prepare, goLive, _ := fixture.chzzk.calls(); prepare != 1 || goLive != 1 {
		t.Fatalf("chzzk calls prepare=%d goLive=%d, want 1/1", prepare, goLive)
	}
}

// 플랜이 허용하지 않는 구성은 방송을 건드리기 전에 거절한다(Beam FHD 동시).
func TestBroadcastModeRejectsDisallowedModeBeforeTouchingBroadcast(t *testing.T) {
	fixture := newModeSwitchFixture(t, time.Second, plan.Beam, session.ResolutionFHD, "youtube")
	status, payload := fixture.putMode(t, `{"targets":["youtube","chzzk"]}`)
	if status != http.StatusForbidden || streamErrorCode(payload) != "plan_simulcast_not_allowed" {
		t.Fatalf("status = %d %q, want 403 plan_simulcast_not_allowed", status, streamErrorCode(payload))
	}
	if prepare, goLive, endLive := fixture.youtube.calls(); prepare != 1 || goLive != 1 || endLive != 0 {
		t.Fatalf("youtube calls prepare=%d goLive=%d endLive=%d, want untouched", prepare, goLive, endLive)
	}
	if fixture.chzzk.prepareCount() != 0 {
		t.Fatal("chzzk must not be prepared")
	}
	payload = getSessionPayload(t, fixture.baseURL, fixture.sessionID, fixture.ownerToken)
	if payload["resolution_switch"] != nil || targetBroadcastPhase(t, payload, "youtube") != "live" {
		t.Fatalf("rejected switch left state %v / youtube %q", payload["resolution_switch"], targetBroadcastPhase(t, payload, "youtube"))
	}
}

func TestBroadcastModeValidatesRequest(t *testing.T) {
	server, _ := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: &stubStreamingProvider{},
	})
	created, ownerToken := createTestSession(t, server.URL, nil)
	fixture := liveSwitchFixture{baseURL: server.URL, sessionID: created.SessionID, ownerToken: ownerToken}
	for _, test := range []struct {
		body   string
		status int
		code   string
	}{
		{`{"targets":[]}`, http.StatusBadRequest, "bad_request"},
		{`{"targets":["soop"]}`, http.StatusBadRequest, "bad_request"},
		// 치지직이 조립되지 않은 배포
		{`{"targets":["chzzk"]}`, http.StatusBadRequest, "bad_request"},
		{`{"resolution":"1080p","targets":["youtube"]}`, http.StatusBadRequest, "bad_request"},
		// 방송 중이 아니면 대상 구성은 방송 준비가 정한다
		{`{"targets":["youtube"]}`, http.StatusConflict, "broadcast_not_live"},
	} {
		if status, payload := fixture.putMode(t, test.body); status != test.status || streamErrorCode(payload) != test.code {
			t.Fatalf("%s = %d %q, want %d %s", test.body, status, streamErrorCode(payload), test.status, test.code)
		}
	}
}
