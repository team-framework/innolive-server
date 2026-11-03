package server

import (
	"net/http"
	"testing"
)

// 앱이 하트비트를 몰아 보내도 행이 그만큼 쌓이면 안 된다. usage_heartbeats에는
// 보존 기간이 없어 반복 요청이 그대로 DB를 키운다(P1-33). 응답과 입력 없음 시계는
// 매번 갱신하고, 저장만 간격을 둔다.
func TestHeartbeatBurstIsStoredOncePerInterval(t *testing.T) {
	store, baseURL := heartbeatTestServer(t)
	sessionID, ownerToken := onDeviceStreamSession(t, baseURL)
	prepareStream(t, baseURL, sessionID, ownerToken, `{}`)
	if response, payload := goLive(t, baseURL, sessionID, ownerToken); response.StatusCode != http.StatusOK {
		t.Fatalf("go live status = %d (payload %v)", response.StatusCode, payload)
	}
	for i := range 5 {
		if response, payload := postHeartbeat(t, baseURL, sessionID, ownerToken, validHeartbeat); response.StatusCode != http.StatusOK {
			t.Fatalf("heartbeat %d status = %d (payload %v)", i, response.StatusCode, payload)
		}
	}
	if store.count() != 1 {
		t.Fatalf("saved = %d after a burst, want 1", store.count())
	}
}
