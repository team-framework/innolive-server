package server

import (
	"context"
	"net/http"
	"testing"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
)

// activeCheckingProvider는 채널의 라이브 방송 id를 돌려주는 스텁이다.
type activeCheckingProvider struct {
	*stubStreamingProvider
	active []string
	quota  *streaming.QuotaMeter
}

func (p activeCheckingProvider) Quota() *streaming.QuotaMeter { return p.quota }

func (p activeCheckingProvider) ActiveBroadcasts(context.Context, uuid.UUID) ([]string, error) {
	return p.active, nil
}

// 채널이 이미 다른 도구로 라이브면 확인을 받고, 확인하면 준비한다(#361).
func TestPrepareStreamAsksBeforeConcurrentYouTubeBroadcast(t *testing.T) {
	stub := &stubStreamingProvider{}
	server, _, application := newStreamTestApplicationWithServer(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: activeCheckingProvider{stubStreamingProvider: stub, active: []string{"obs-live", "innolive-own"}, quota: streaming.NewQuotaMeter()},
	})
	application.ownBroadcasts.add("innolive-own")
	created, ownerToken := createTestSession(t, server.URL, nil)
	putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`)

	response, payload := prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	if response.StatusCode != http.StatusConflict || streamErrorCode(payload) != "channel_already_live" {
		t.Fatalf("prepare = %d %v, want 409 channel_already_live", response.StatusCode, payload)
	}
	// InnoLive가 만든 방송은 세지 않는다.
	details, _ := payload["error"].(map[string]any)["details"].(map[string]any)
	if details["active_broadcasts"] != float64(1) {
		t.Fatalf("details = %v, want 1 foreign broadcast", details)
	}
	stub.mu.Lock()
	calls := stub.prepareCalls
	stub.mu.Unlock()
	if calls != 0 {
		t.Fatalf("prepare calls = %d, want none before confirmation", calls)
	}

	_, payload = prepareStream(t, server.URL, created.SessionID, ownerToken, `{"allow_concurrent":true}`)
	if streamErrorCode(payload) == "channel_already_live" {
		t.Fatalf("confirmed prepare still asks: %v", payload)
	}
	stub.mu.Lock()
	calls = stub.prepareCalls
	stub.mu.Unlock()
	if calls != 1 {
		t.Fatalf("prepare calls = %d, want 1 after confirmation", calls)
	}
}

// 우리 방송만 active면 묻지 않는다 — 방금 끝낸 방송은 autoStop 전까지 active다.
func TestPrepareStreamIgnoresOwnActiveBroadcast(t *testing.T) {
	stub := &stubStreamingProvider{}
	server, _, application := newStreamTestApplicationWithServer(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: activeCheckingProvider{stubStreamingProvider: stub, active: []string{"innolive-own"}, quota: streaming.NewQuotaMeter()},
	})
	application.ownBroadcasts.add("innolive-own")
	created, ownerToken := createTestSession(t, server.URL, nil)
	putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`)
	if _, payload := prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`); streamErrorCode(payload) == "channel_already_live" {
		t.Fatalf("own broadcast triggered the confirmation: %v", payload)
	}
}

// 쿼터가 80%를 넘으면 채널 라이브 확인을 건너뛰고 바로 준비한다(#361).
func TestPrepareStreamSkipsLiveCheckWhenQuotaLow(t *testing.T) {
	stub := &stubStreamingProvider{}
	meter := streaming.NewQuotaMeter()
	for i := 0; i < 160; i++ {
		meter.Record(http.MethodPost)
	}
	server, _, _ := newStreamTestApplicationWithServer(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: activeCheckingProvider{stubStreamingProvider: stub, active: []string{"obs-live"}, quota: meter},
	})
	created, ownerToken := createTestSession(t, server.URL, nil)
	putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`)
	if _, payload := prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`); streamErrorCode(payload) == "channel_already_live" {
		t.Fatalf("low quota must skip the live check: %v", payload)
	}
}
