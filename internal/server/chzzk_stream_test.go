package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/streaming"
)

func chzzkPrepared() streaming.PreparedBroadcast {
	return streaming.PreparedBroadcast{
		Provider:  auth.StreamingProviderChzzk,
		IngestURL: streaming.ChzzkIngestURL + "/secret-key",
	}
}

func streamPhase(payload map[string]any) (status, phase string) {
	stream, _ := payload["stream"].(map[string]any)
	status, _ = stream["status"].(string)
	phase, _ = stream["broadcast_phase"].(string)
	return status, phase
}

// TestChzzkPrepareDoesNotAttachEgress: 치지직은 RTMP 연결이 곧 공개 방송이라
// (D1) 준비 단계에서 egress를 붙이면 안 된다. 트랙이 없는 세션에서 유튜브
// 준비는 egress 부착 시도로 409지만, 치지직 준비는 200이고 phase만 prepared다.
func TestChzzkPrepareDoesNotAttachEgress(t *testing.T) {
	chzzk, baseURL := chzzkTestApplication(t)
	chzzk.prepared = chzzkPrepared()
	created, ownerToken := chzzkTestSession(t, baseURL)

	response, payload := prepareStream(t, baseURL, created.SessionID, ownerToken, `{}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 without a video track (payload %v)", response.StatusCode, payload)
	}
	status, phase := streamPhase(payload)
	if phase != "prepared" || status == "streaming" {
		t.Fatalf("stream = %q/%q, want prepared without egress", status, phase)
	}
	// 스트림키가 실린 ingest URL은 응답에 나가면 안 된다.
	if encoded, _ := json.Marshal(payload); strings.Contains(string(encoded), "secret-key") {
		t.Fatalf("response leaks ingest url: %s", encoded)
	}
}

// TestChzzkGoLiveAttachesEgress: 라이브 전환이 egress를 붙인다. 트랙이 없어
// 실패하면 플랫폼에 되돌릴 것은 없고 준비 상태로만 돌아가야 한다.
func TestChzzkGoLiveAttachesEgress(t *testing.T) {
	chzzk, baseURL := chzzkTestApplication(t)
	chzzk.prepared = chzzkPrepared()
	created, ownerToken := chzzkTestSession(t, baseURL)
	if response, payload := prepareStream(t, baseURL, created.SessionID, ownerToken, `{}`); response.StatusCode != http.StatusOK {
		t.Fatalf("prepare status = %d (payload %v)", response.StatusCode, payload)
	}

	response, payload := goLive(t, baseURL, created.SessionID, ownerToken)
	if response.StatusCode != http.StatusConflict || streamErrorCode(payload) != "conflict" {
		t.Fatalf("go live = %d %q, want 409 conflict from the missing video track", response.StatusCode, streamErrorCode(payload))
	}
	if chzzk.goLiveCalls != 1 {
		t.Fatalf("go live calls = %d, want the provider called before egress", chzzk.goLiveCalls)
	}
	if _, phase := streamPhase(getSessionPayload(t, baseURL, created.SessionID, ownerToken)); phase != "prepared" {
		t.Fatalf("phase = %q, want prepared restored after a failed egress attach", phase)
	}
	if stopCalls, _ := chzzk.stopped(); stopCalls != 0 {
		t.Fatalf("stop calls = %d, want nothing discarded on the platform", stopCalls)
	}
}

// TestChzzkStopReleasesPreparedBroadcast: egress 없이 준비만 된 방송은
// 중지로 놓아줄 수 있어야 한다 — 아니면 라이브 전환이나 세션 삭제 외에는
// 준비 상태를 빠져나갈 길이 없다.
func TestChzzkStopReleasesPreparedBroadcast(t *testing.T) {
	chzzk, baseURL := chzzkTestApplication(t)
	chzzk.prepared = chzzkPrepared()
	created, ownerToken := chzzkTestSession(t, baseURL)
	if response, payload := prepareStream(t, baseURL, created.SessionID, ownerToken, `{}`); response.StatusCode != http.StatusOK {
		t.Fatalf("prepare status = %d (payload %v)", response.StatusCode, payload)
	}

	response, payload := postStream(t, baseURL, created.SessionID, ownerToken, "stop", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, want 200 (payload %v)", response.StatusCode, payload)
	}
	if _, phase := streamPhase(getSessionPayload(t, baseURL, created.SessionID, ownerToken)); phase != "idle" {
		t.Fatalf("phase = %q, want idle after release", phase)
	}
	// 놓아준 뒤에는 다시 준비할 수 있다.
	if response, payload := prepareStream(t, baseURL, created.SessionID, ownerToken, `{}`); response.StatusCode != http.StatusOK {
		t.Fatalf("re-prepare status = %d (payload %v)", response.StatusCode, payload)
	}
	if chzzk.prepareCalls != 2 {
		t.Fatalf("prepare calls = %d, want 2", chzzk.prepareCalls)
	}
}

// TestChzzkBroadcastDefaultsIncludeCategoryTypeAndTags: 치지직 기본값은
// category_type·tags를 싣고, 유튜브 기본값 응답에는 그 키가 없어야 한다.
func TestChzzkBroadcastDefaultsIncludeCategoryTypeAndTags(t *testing.T) {
	chzzk, baseURL := chzzkTestApplication(t)
	chzzk.defaults = streaming.BroadcastDefaults{Title: "채널 제목", CategoryType: "ETC", CategoryID: "talk", Tags: []string{"a"}}
	created, ownerToken := chzzkTestSession(t, baseURL)

	request, err := http.NewRequest(http.MethodGet, baseURL+"/sessions/"+created.SessionID+"/broadcast/defaults?provider=chzzk", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Session-Owner-Token", ownerToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&payload)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (payload %v)", response.StatusCode, payload)
	}
	if payload["category_type"] != "ETC" || payload["category_id"] != "talk" {
		t.Fatalf("defaults = %v", payload)
	}
	if tags, _ := payload["tags"].([]any); len(tags) != 1 {
		t.Fatalf("tags = %v", payload["tags"])
	}

	youtubeCreated, youtubeToken := createTestSession(t, baseURL, nil)
	_, youtubePayload := getBroadcastDefaults(t, baseURL, youtubeCreated.SessionID, youtubeToken)
	if _, ok := youtubePayload["category_type"]; ok {
		t.Fatalf("youtube defaults must not carry category_type: %v", youtubePayload)
	}
	if _, ok := youtubePayload["tags"]; ok {
		t.Fatalf("youtube defaults must not carry tags: %v", youtubePayload)
	}
}
