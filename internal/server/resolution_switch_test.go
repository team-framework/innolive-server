package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/streaming"

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
}

func newLiveSwitchFixture(t *testing.T, chzzkWait time.Duration) liveSwitchFixture {
	t.Helper()
	previousWait, previousRetry := resolutionSwitchChzzkWait, resolutionSwitchRetryInterval
	resolutionSwitchChzzkWait, resolutionSwitchRetryInterval = chzzkWait, 20*time.Millisecond
	t.Cleanup(func() { resolutionSwitchChzzkWait, resolutionSwitchRetryInterval = previousWait, previousRetry })

	output := t.TempDir()
	youtube := &stubStreamingProvider{prepared: streaming.PreparedBroadcast{Provider: auth.StreamingProviderYouTube, BroadcastID: "yt-1", IngestURL: filepath.Join(output, "youtube.flv")}}
	chzzk := &stubStreamingProvider{prepared: streaming.PreparedBroadcast{Provider: auth.StreamingProviderChzzk, IngestURL: filepath.Join(output, "chzzk.flv")}}
	server, _ := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: youtube,
		auth.StreamingProviderChzzk:   chzzk,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)
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
	for _, body := range []string{`{}`, `{"provider":"chzzk"}`} {
		if response, payload := prepareStream(t, server.URL, created.SessionID, ownerToken, body); response.StatusCode != http.StatusOK {
			t.Fatalf("prepare %s = %d %v", body, response.StatusCode, payload)
		}
	}
	if response, payload := goLive(t, server.URL, created.SessionID, ownerToken); response.StatusCode != http.StatusOK {
		t.Fatalf("go live = %d %v", response.StatusCode, payload)
	}
	return liveSwitchFixture{baseURL: server.URL, sessionID: created.SessionID, ownerToken: ownerToken, youtube: youtube, chzzk: chzzk}
}

func (f liveSwitchFixture) putResolution(t *testing.T, resolution string) (int, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut, f.baseURL+"/sessions/"+f.sessionID+"/broadcast-resolution", bytes.NewBufferString(`{"resolution":"`+resolution+`"}`))
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
