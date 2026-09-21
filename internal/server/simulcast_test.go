package server

import (
	"net/http"
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
