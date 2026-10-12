package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/streaming"
	"inno-live-server/internal/usage"
)

type fakeHeartbeatStore struct {
	mu    sync.Mutex
	saved []usage.Heartbeat
}

func (f *fakeHeartbeatStore) SaveHeartbeat(_ context.Context, heartbeat usage.Heartbeat) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, heartbeat)
	return nil
}

func (f *fakeHeartbeatStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.saved)
}

const validHeartbeat = `{
  "sent_at": "2026-10-12T08:30:00Z",
  "targets": [{"provider": "youtube", "connected": true, "bitrate_kbps": 5200}],
  "video": {"fps": 29.8, "dropped_frames": 12},
  "anonymization": {"enabled": true, "failed_frames": 0},
  "device": {"thermal": "fair"},
  "platform": "ios",
  "model": "iPhone15,3"
}`

func heartbeatTestServer(t *testing.T) (*fakeHeartbeatStore, string) {
	t.Helper()
	youtube := &stubStreamingProvider{prepared: streaming.PreparedBroadcast{
		Provider: auth.StreamingProviderYouTube, BroadcastID: "b1", IngestURL: "rtmps://a.rtmp.youtube.com/live2/secret-key",
	}}
	server, _, app := newStreamTestApplicationWithServer(t, map[auth.StreamingProvider]streaming.Provider{auth.StreamingProviderYouTube: youtube})
	store := &fakeHeartbeatStore{}
	app.SetHeartbeatStore(store)
	return store, server.URL
}

func postHeartbeat(t *testing.T, baseURL, sessionID, ownerToken, body string) (*http.Response, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, baseURL+"/sessions/"+sessionID+"/heartbeat", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Session-Owner-Token", ownerToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	payload := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&payload)
	return response, payload
}

// TestHeartbeatSavedForLiveOnDeviceSession: 라이브 중인 온디바이스 세션의
// 보고는 저장되고, 응답은 종료 대상·알림·남은 시간을 싣는다.
func TestHeartbeatSavedForLiveOnDeviceSession(t *testing.T) {
	store, baseURL := heartbeatTestServer(t)
	sessionID, ownerToken := onDeviceStreamSession(t, baseURL)
	prepareStream(t, baseURL, sessionID, ownerToken, `{}`)
	if response, payload := goLive(t, baseURL, sessionID, ownerToken); response.StatusCode != http.StatusOK {
		t.Fatalf("go live status = %d (payload %v)", response.StatusCode, payload)
	}

	response, payload := postHeartbeat(t, baseURL, sessionID, ownerToken, validHeartbeat)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat status = %d (payload %v)", response.StatusCode, payload)
	}
	if _, ok := payload["stop_targets"].([]any); !ok {
		t.Fatalf("stop_targets = %v, want an array", payload["stop_targets"])
	}
	if store.count() != 1 {
		t.Fatalf("saved = %d, want 1", store.count())
	}
	saved := store.saved[0]
	if saved.ConnectedTargets != 1 || saved.VideoFPS != 29.8 || saved.Platform != "ios" || saved.Payload == "" {
		t.Fatalf("saved = %+v", saved)
	}
}

// TestHeartbeatRejected: 소유자 불일치·server 모드·라이브 전·필수 필드 누락은
// 저장하지 않는다.
func TestHeartbeatRejected(t *testing.T) {
	store, baseURL := heartbeatTestServer(t)
	onDeviceID, onDeviceToken := onDeviceStreamSession(t, baseURL)

	if response, _ := postHeartbeat(t, baseURL, onDeviceID, "wrong-token", validHeartbeat); response.StatusCode == http.StatusOK {
		t.Fatal("heartbeat with wrong owner token accepted")
	}
	if response, payload := postHeartbeat(t, baseURL, onDeviceID, onDeviceToken, validHeartbeat); response.StatusCode != http.StatusConflict || streamErrorCode(payload) != "heartbeat_not_applicable" {
		t.Fatalf("before go live = %d %q, want 409 heartbeat_not_applicable", response.StatusCode, streamErrorCode(payload))
	}
	prepareStream(t, baseURL, onDeviceID, onDeviceToken, `{}`)
	goLive(t, baseURL, onDeviceID, onDeviceToken)
	if response, payload := postHeartbeat(t, baseURL, onDeviceID, onDeviceToken, `{"sent_at":"2026-10-12T08:30:00Z","targets":[{"provider":"youtube","connected":true}],"platform":"ios","model":"x"}`); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing video = %d (payload %v), want 400", response.StatusCode, payload)
	}
	serverSession, serverToken := createTestSession(t, baseURL, nil)
	if response, payload := postHeartbeat(t, baseURL, serverSession.SessionID, serverToken, validHeartbeat); response.StatusCode != http.StatusConflict || streamErrorCode(payload) != "heartbeat_not_applicable" {
		t.Fatalf("server mode = %d %q, want 409 heartbeat_not_applicable", response.StatusCode, streamErrorCode(payload))
	}
	if store.count() != 0 {
		t.Fatalf("saved = %d, want nothing saved", store.count())
	}
}
