package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/streaming"
)

func onDeviceStreamSession(t *testing.T, baseURL string) (string, string) {
	t.Helper()
	created, ownerToken := createTestSession(t, baseURL, map[string]string{"ai_processing": "on_device"})
	putBroadcast(t, baseURL, created.SessionID, ownerToken, `{"made_for_kids":false}`)
	return created.SessionID, ownerToken
}

// TestOnDeviceYouTubePrepareReturnsIngestURL: 온디바이스 세션은 앱이 직접
// 송출한다(#438). 트랙이 없어도 egress를 붙이지 않으므로 준비가 성공하고,
// 유튜브 ingest URL은 준비 응답으로 나간다. 라이브 전환 후 중지할 수 있다.
func TestOnDeviceYouTubePrepareReturnsIngestURL(t *testing.T) {
	youtube := &stubStreamingProvider{prepared: streaming.PreparedBroadcast{
		Provider: auth.StreamingProviderYouTube, BroadcastID: "b1", IngestURL: "rtmps://a.rtmp.youtube.com/live2/secret-key",
	}}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{auth.StreamingProviderYouTube: youtube})
	sessionID, ownerToken := onDeviceStreamSession(t, server.URL)

	response, payload := prepareStream(t, server.URL, sessionID, ownerToken, `{}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("prepare status = %d (payload %v)", response.StatusCode, payload)
	}
	if payload["ingest_url"] != youtube.prepared.IngestURL {
		t.Fatalf("ingest_url = %v, want the prepared ingest url", payload["ingest_url"])
	}
	response, payload = goLive(t, server.URL, sessionID, ownerToken)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("go live status = %d (payload %v)", response.StatusCode, payload)
	}
	if _, ok := payload["direct_ingest"]; ok {
		t.Fatalf("youtube ingest url repeated at go live: %v", payload)
	}
	if response, payload := postStream(t, server.URL, sessionID, ownerToken, "pause", ""); response.StatusCode != http.StatusConflict || streamErrorCode(payload) != "on_device_processing" {
		t.Fatalf("pause = %d %q, want 409 on_device_processing", response.StatusCode, streamErrorCode(payload))
	}
	response, payload = postStream(t, server.URL, sessionID, ownerToken, "stop", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d (payload %v)", response.StatusCode, payload)
	}
	if _, phase := streamPhase(getSessionPayload(t, server.URL, sessionID, ownerToken)); phase != "idle" {
		t.Fatalf("phase = %q, want idle after stop", phase)
	}
}

// TestOnDeviceChzzkIngestURLOnlyAtGoLive: 치지직은 연결이 곧 공개 방송이라(D1)
// 준비 응답에는 싣지 않고 라이브 전환 응답에서만 넘긴다.
func TestOnDeviceChzzkIngestURLOnlyAtGoLive(t *testing.T) {
	chzzk := &stubStreamingProvider{prepared: chzzkPrepared()}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: &stubStreamingProvider{},
		auth.StreamingProviderChzzk:   chzzk,
	})
	sessionID, ownerToken := onDeviceStreamSession(t, server.URL)

	response, payload := prepareStream(t, server.URL, sessionID, ownerToken, `{"provider":"chzzk"}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("prepare status = %d (payload %v)", response.StatusCode, payload)
	}
	if encoded, _ := json.Marshal(payload); strings.Contains(string(encoded), "secret-key") {
		t.Fatalf("prepare response leaks chzzk ingest url: %s", encoded)
	}
	response, payload = goLive(t, server.URL, sessionID, ownerToken)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("go live status = %d (payload %v)", response.StatusCode, payload)
	}
	entries, _ := payload["direct_ingest"].([]any)
	if len(entries) != 1 {
		t.Fatalf("direct_ingest = %v, want one chzzk entry", payload["direct_ingest"])
	}
	entry, _ := entries[0].(map[string]any)
	if entry["provider"] != "chzzk" || entry["ingest_url"] != chzzk.prepared.IngestURL {
		t.Fatalf("direct_ingest entry = %v", entry)
	}
}

// TestServerModePrepareHidesIngestURL: server 모드 세션은 서버가 송출하므로
// 스트림 키가 실린 ingest URL을 어떤 응답에도 싣지 않는다.
func TestServerModePrepareHidesIngestURL(t *testing.T) {
	chzzk := &stubStreamingProvider{prepared: chzzkPrepared()}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: &stubStreamingProvider{},
		auth.StreamingProviderChzzk:   chzzk,
	})
	created, ownerToken := chzzkTestSession(t, server.URL)

	response, payload := prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("prepare status = %d (payload %v)", response.StatusCode, payload)
	}
	if _, ok := payload["ingest_url"]; ok {
		t.Fatalf("server mode prepare returned ingest_url: %v", payload)
	}
	// 트랙이 없어 egress 부착이 실패하는 경로에서도 키가 나가지 않는다.
	_, payload = goLive(t, server.URL, created.SessionID, ownerToken)
	if encoded, _ := json.Marshal(payload); strings.Contains(string(encoded), "secret-key") {
		t.Fatalf("go live response leaks ingest url: %s", encoded)
	}
}

// TestOnDeviceIngestURLRequiresOwner: 소유자가 아니면 준비 자체가 거부되어
// ingest URL에 닿을 수 없다.
func TestOnDeviceIngestURLRequiresOwner(t *testing.T) {
	youtube := &stubStreamingProvider{prepared: streaming.PreparedBroadcast{
		Provider: auth.StreamingProviderYouTube, BroadcastID: "b1", IngestURL: "rtmps://a.rtmp.youtube.com/live2/secret-key",
	}}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{auth.StreamingProviderYouTube: youtube})
	sessionID, _ := onDeviceStreamSession(t, server.URL)

	response, payload := prepareStream(t, server.URL, sessionID, "wrong-token", `{}`)
	if response.StatusCode == http.StatusOK {
		t.Fatalf("prepare with wrong owner token succeeded (payload %v)", payload)
	}
	if youtube.prepareCalls != 0 {
		t.Fatalf("prepare calls = %d, want the platform untouched", youtube.prepareCalls)
	}
}
