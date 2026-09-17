package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"
)

// createSessionWithProvider는 POST /sessions에 provider를 실어 보낸다. 빈
// 문자열이면 필드 자체를 생략해 provider를 모르는 기존 클라이언트를 흉내낸다.
func createSessionWithProvider(t *testing.T, baseURL, provider string) (*http.Response, map[string]any) {
	t.Helper()
	body := map[string]any{}
	if provider != "" {
		body["provider"] = provider
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/sessions", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{}
	_ = json.Unmarshal(data, &payload)
	return response, payload
}

// TestCreateSessionDefaultsToYouTubeProvider: provider를 보내지 않는 기존
// 클라이언트는 종전대로 유튜브 세션을 얻는다(#227).
func TestCreateSessionDefaultsToYouTubeProvider(t *testing.T) {
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: &stubStreamingProvider{},
	})

	response, payload := createSessionWithProvider(t, server.URL, "")
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (payload %v)", response.StatusCode, payload)
	}
	if payload["provider"] != string(auth.StreamingProviderYouTube) {
		t.Fatalf("provider = %v, want youtube", payload["provider"])
	}
}

// TestCreateSessionAcceptsKnownProvider: 알려진 플랫폼은 세션 값으로 남는다.
// 이 배포에 치지직 송출이 조립돼 있지 않아도 세션 생성은 통과해야 한다 —
// 조립 여부는 prepare가 501로 답하는 종전 계약이다.
func TestCreateSessionAcceptsKnownProvider(t *testing.T) {
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: &stubStreamingProvider{},
	})

	response, payload := createSessionWithProvider(t, server.URL, string(auth.StreamingProviderChzzk))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (payload %v)", response.StatusCode, payload)
	}
	if payload["provider"] != string(auth.StreamingProviderChzzk) {
		t.Fatalf("provider = %v, want chzzk", payload["provider"])
	}
}

// TestCreateSessionRejectsUnknownProvider: 알려지지 않은 식별자는 세션을
// 만들기 전에 400이다.
func TestCreateSessionRejectsUnknownProvider(t *testing.T) {
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: &stubStreamingProvider{},
	})

	response, payload := createSessionWithProvider(t, server.URL, "twitch")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (payload %v)", response.StatusCode, payload)
	}
	details, _ := payload["error"].(map[string]any)["details"].(map[string]any)
	if details["provider"] != "twitch" {
		t.Fatalf("details = %v, want the rejected provider echoed", details)
	}
}

// TestPrepareStreamRejectsProviderMismatch: prepare의 provider는 더 이상
// 선택이 아니라 세션 값과 일치하는지 확인하는 용도다. 어긋나면 플랫폼을
// 부르기 전에 400이다.
func TestPrepareStreamRejectsProviderMismatch(t *testing.T) {
	provider := &stubStreamingProvider{}
	// 치지직도 등록해 둔다 — 미등록(501)이 아니라 불일치(400)를 보는 테스트다.
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
		auth.StreamingProviderChzzk:   &stubStreamingProvider{},
	})
	created, ownerToken := createTestSession(t, server.URL, nil)
	if created.Provider != string(auth.StreamingProviderYouTube) {
		t.Fatalf("session provider = %q, want youtube", created.Provider)
	}

	response, payload := prepareStream(t, server.URL, created.SessionID, ownerToken, `{"provider":"chzzk"}`)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (payload %v)", response.StatusCode, payload)
	}
	if provider.prepareCalls != 0 {
		t.Fatalf("prepare calls = %d, want no platform call", provider.prepareCalls)
	}
}

// TestPrepareStreamUsesSessionProvider: provider를 생략한 prepare는 세션이
// 들고 있는 플랫폼으로 간다 — 유튜브가 아니어도 그렇다.
func TestPrepareStreamUsesSessionProvider(t *testing.T) {
	youtube := &stubStreamingProvider{}
	chzzk := &stubStreamingProvider{prepareErr: auth.ErrStreamingNotConnected}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: youtube,
		auth.StreamingProviderChzzk:   chzzk,
	})

	response, payload := createSessionWithProvider(t, server.URL, string(auth.StreamingProviderChzzk))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (payload %v)", response.StatusCode, payload)
	}
	var created struct {
		session.Response
		OwnerToken string `json:"owner_token"`
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &created); err != nil {
		t.Fatal(err)
	}

	putBroadcast(t, server.URL, created.SessionID, created.OwnerToken, `{"made_for_kids":true}`)
	prepareStream(t, server.URL, created.SessionID, created.OwnerToken, `{}`)
	if chzzk.prepareCalls != 1 {
		t.Fatalf("chzzk prepare calls = %d, want 1", chzzk.prepareCalls)
	}
	if youtube.prepareCalls != 0 {
		t.Fatalf("youtube prepare calls = %d, want the session provider to win", youtube.prepareCalls)
	}
}
