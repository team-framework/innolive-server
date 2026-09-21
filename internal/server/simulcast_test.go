package server

import (
	"net/http"
	"strings"
	"testing"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"
)

// markPrepared는 플랫폼 왕복 없이 대상 하나를 준비 상태로 세운다. 유튜브
// 준비는 egress 부착까지 하므로(트랙이 필요하다) 라이브 전환만 보는
// 테스트는 이 경로로 상태를 만든다.
func markPrepared(t *testing.T, manager *session.Manager, sessionID string, provider auth.StreamingProvider, ingestURL string) {
	t.Helper()
	if _, err := manager.BeginBroadcastPrepare(sessionID, string(provider)); err != nil {
		t.Fatalf("%s BeginBroadcastPrepare() error = %v", provider, err)
	}
	if _, err := manager.MarkBroadcastPrepared(sessionID, session.PlatformBroadcast{
		Provider:    string(provider),
		BroadcastID: string(provider) + "-bid",
		IngestURL:   ingestURL,
	}, string(provider)); err != nil {
		t.Fatalf("%s MarkBroadcastPrepared() error = %v", provider, err)
	}
}

func goLiveFailedTargets(t *testing.T, payload map[string]any) map[string]string {
	t.Helper()
	failed := map[string]string{}
	entries, ok := payload["failed_targets"].([]any)
	if !ok {
		return failed
	}
	for _, entry := range entries {
		target, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("failed_targets entry = %v, want an object", entry)
		}
		provider, _ := target["provider"].(string)
		code, _ := target["code"].(string)
		failed[provider] = code
	}
	return failed
}

// TestGoLiveFiresEveryPreparedTarget: 골라이브는 준비된 대상을 모두 발사한다.
// 동시 송출의 발사 지점은 유튜브의 2단계 버튼 하나뿐이다(D1).
func TestGoLiveFiresEveryPreparedTarget(t *testing.T) {
	youtube := &stubStreamingProvider{}
	chzzk := &stubStreamingProvider{}
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: youtube,
		auth.StreamingProviderChzzk:   chzzk,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)
	markPrepared(t, manager, created.SessionID, auth.StreamingProviderYouTube, "")
	markPrepared(t, manager, created.SessionID, auth.StreamingProviderChzzk, streaming.ChzzkIngestURL+"/key")

	response, payload := goLive(t, server.URL, created.SessionID, ownerToken)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (payload %v)", response.StatusCode, payload)
	}
	if youtube.goLiveCalls != 1 || chzzk.goLiveCalls != 1 {
		t.Fatalf("go live calls = youtube %d / chzzk %d, want 1 each", youtube.goLiveCalls, chzzk.goLiveCalls)
	}
}

// TestGoLiveKeepsOtherTargetLiveWhenOneFails: 한쪽이 실패해도 나머지는 그대로
// 간다(사용자 결정). 치지직은 재연결이 곧 새 방송이라 롤백이 깔끔하지 않고,
// 이미 붙은 유튜브 시청자를 끊는 손실이 한쪽 실패보다 크다.
func TestGoLiveKeepsOtherTargetLiveWhenOneFails(t *testing.T) {
	youtube := &stubStreamingProvider{}
	chzzk := &stubStreamingProvider{goLiveErr: auth.ErrStreamingReconnectRequired}
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: youtube,
		auth.StreamingProviderChzzk:   chzzk,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)
	markPrepared(t, manager, created.SessionID, auth.StreamingProviderYouTube, "")
	markPrepared(t, manager, created.SessionID, auth.StreamingProviderChzzk, streaming.ChzzkIngestURL+"/key")

	response, payload := goLive(t, server.URL, created.SessionID, ownerToken)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with one target live (payload %v)", response.StatusCode, payload)
	}
	failed := goLiveFailedTargets(t, payload)
	if failed[string(auth.StreamingProviderChzzk)] != "streaming_reconnect_required" {
		t.Fatalf("failed targets = %v, want chzzk with its reason", failed)
	}
	if _, ok := failed[string(auth.StreamingProviderYouTube)]; ok {
		t.Fatalf("failed targets = %v, want youtube to have gone live", failed)
	}
	// 살아남은 대상은 라이브, 실패한 대상은 준비 상태로 되돌아간다.
	session := getSessionPayload(t, server.URL, created.SessionID, ownerToken)
	if phase := targetBroadcastPhase(t, session, string(auth.StreamingProviderYouTube)); phase != "live" {
		t.Fatalf("youtube phase = %q, want live", phase)
	}
	if phase := targetBroadcastPhase(t, session, string(auth.StreamingProviderChzzk)); phase != "prepared" {
		t.Fatalf("chzzk phase = %q, want prepared restored", phase)
	}
}

// TestGoLiveFailsWhenEveryTargetFails: 전부 실패하면 요청 자체가 실패다 —
// 대상이 하나뿐인 단독 송출의 오류 응답이 종전 그대로여야 한다.
func TestGoLiveFailsWhenEveryTargetFails(t *testing.T) {
	youtube := &stubStreamingProvider{goLiveErr: streaming.ErrBroadcastNotReady}
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: youtube,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)
	markPrepared(t, manager, created.SessionID, auth.StreamingProviderYouTube, "")

	response, payload := goLive(t, server.URL, created.SessionID, ownerToken)
	if response.StatusCode != http.StatusConflict || streamErrorCode(payload) != "broadcast_not_ready" {
		t.Fatalf("go live = %d %q, want 409 broadcast_not_ready", response.StatusCode, streamErrorCode(payload))
	}
}

// TestControlRejectsUnknownProviderWithoutCreatingTarget: 제어·설정
// 엔드포인트의 provider는 세션에 닿기 전에 걸러야 한다. 세션의 대상 맵은
// 요청한 이름으로 대상을 만들어 주므로, 거르지 않으면 임의의 문자열이 그대로
// 세션에 남아 응답에 실리고 맵이 무한히 자란다.
func TestControlRejectsUnknownProviderWithoutCreatingTarget(t *testing.T) {
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: &stubStreamingProvider{},
	})
	created, ownerToken := createTestSession(t, server.URL, nil)

	for _, action := range []string{"stop", "pause", "resume"} {
		response, payload := postStream(t, server.URL, created.SessionID, ownerToken, action+"?provider=bogus", "")
		if response.StatusCode != http.StatusBadRequest || streamErrorCode(payload) != "bad_request" {
			t.Fatalf("%s?provider=bogus = %d %q, want 400 bad_request", action, response.StatusCode, streamErrorCode(payload))
		}
	}
	request, err := http.NewRequest(http.MethodPut, server.URL+"/sessions/"+created.SessionID+"/broadcast?provider=bogus", strings.NewReader(`{"made_for_kids":false}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Session-Owner-Token", ownerToken)
	putResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer putResponse.Body.Close()
	if putResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT /broadcast?provider=bogus = %d, want 400", putResponse.StatusCode)
	}

	// 거절된 요청은 세션에 흔적을 남기지 않는다 — 기본 대상 하나뿐이어야 한다.
	targets, _ := getSessionPayload(t, server.URL, created.SessionID, ownerToken)["targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("targets = %v, want only the default target", targets)
	}
}

// TestControlTargetStartsIdleNotEmptyPhase: 지연 생성된 대상도 응답 계약의
// 값 집합 안에 있어야 한다. 빈 문자열이면 클라이언트가 단계로 분기할 수 없다.
func TestControlTargetStartsIdleNotEmptyPhase(t *testing.T) {
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: &stubStreamingProvider{},
		auth.StreamingProviderChzzk:   &stubStreamingProvider{},
	})
	created, ownerToken := createTestSession(t, server.URL, nil)

	// 유튜브 기본 세션에서 준비 없이 치지직 대상을 건드린다.
	if response, payload := postStream(t, server.URL, created.SessionID, ownerToken, "stop?provider=chzzk", ""); response.StatusCode != http.StatusConflict {
		t.Fatalf("stop?provider=chzzk = %d, want 409 stream_not_active (payload %v)", response.StatusCode, payload)
	}
	phase := targetBroadcastPhase(t, getSessionPayload(t, server.URL, created.SessionID, ownerToken), string(auth.StreamingProviderChzzk))
	if phase != string(session.BroadcastPhaseIdle) {
		t.Fatalf("chzzk target phase = %q, want idle", phase)
	}
}
