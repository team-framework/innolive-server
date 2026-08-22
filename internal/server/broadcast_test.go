package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"
)

func putBroadcast(t *testing.T, baseURL, sessionID, ownerToken, body string) (*http.Response, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut, baseURL+"/sessions/"+sessionID+"/broadcast", bytes.NewBufferString(body))
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

func getSessionPayload(t *testing.T, baseURL, sessionID, ownerToken string) map[string]any {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/sessions/"+sessionID, nil)
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
	return payload
}

// TestPutBroadcastStoresAndReturnsSettings: 저장 후 조회하면 같은 값이 나오고,
// 썸네일은 원본 대신 메타데이터만 노출돼야 한다.
func TestPutBroadcastStoresAndReturnsSettings(t *testing.T) {
	server := newStreamTestApplication(t, nil)
	created, ownerToken := createTestSession(t, server.URL, nil)

	thumbnail := base64.StdEncoding.EncodeToString([]byte("jpeg-bytes"))
	response, payload := putBroadcast(t, server.URL, created.SessionID, ownerToken,
		`{"title":"내 방송","description":"설명","privacy":"private","made_for_kids":false,"category_id":"20","thumbnail":{"mime":"image/jpeg","data_base64":"`+thumbnail+`"}}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (payload %v)", response.StatusCode, payload)
	}

	broadcast, _ := getSessionPayload(t, server.URL, created.SessionID, ownerToken)["broadcast"].(map[string]any)
	if broadcast == nil {
		t.Fatal("GET /sessions/{id} did not return broadcast settings")
	}
	if broadcast["title"] != "내 방송" || broadcast["description"] != "설명" ||
		broadcast["privacy"] != "private" || broadcast["category_id"] != "20" ||
		broadcast["made_for_kids"] != false {
		t.Fatalf("broadcast = %v", broadcast)
	}
	thumbnailResponse, _ := broadcast["thumbnail"].(map[string]any)
	if thumbnailResponse["mime"] != "image/jpeg" || thumbnailResponse["bytes"] != float64(len("jpeg-bytes")) {
		t.Fatalf("thumbnail = %v", thumbnailResponse)
	}

	// 경계값은 허용돼야 한다(100자 제목, 5000자 설명).
	if response, payload := putBroadcast(t, server.URL, created.SessionID, ownerToken,
		`{"title":"`+strings.Repeat("가", 100)+`","description":"`+strings.Repeat("a", 5000)+`"}`); response.StatusCode != http.StatusOK {
		t.Fatalf("boundary lengths rejected: status = %d (payload %v)", response.StatusCode, payload)
	}
}

// TestPutBroadcastRejectsInvalidSettings: 검증 실패는 400이고 어떤 필드인지
// 알려줘야 한다. 이 경로에는 플랫폼 호출이 없다.
func TestPutBroadcastRejectsInvalidSettings(t *testing.T) {
	oversizedThumbnail := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, session.MaxYouTubeThumbnailBytes+1))
	cases := []struct {
		name      string
		body      string
		wantField string
	}{
		{"title too long", `{"title":"` + strings.Repeat("가", 101) + `","made_for_kids":false}`, "title"},
		{"description too long", `{"description":"` + strings.Repeat("a", 5001) + `"}`, "description"},
		{"unknown privacy", `{"privacy":"friends"}`, "privacy"},
		{"non numeric category", `{"category_id":"music"}`, "category_id"},
		{"thumbnail mime", `{"thumbnail":{"mime":"image/gif","data_base64":"AAAA"}}`, "thumbnail.mime"},
		{"thumbnail too large", `{"thumbnail":{"mime":"image/png","data_base64":"` + oversizedThumbnail + `"}}`, "thumbnail.data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &stubStreamingProvider{}
			server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
				auth.StreamingProviderYouTube: provider,
			})
			created, ownerToken := createTestSession(t, server.URL, nil)
			response, payload := putBroadcast(t, server.URL, created.SessionID, ownerToken, tc.body)
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (payload %v)", response.StatusCode, payload)
			}
			details, _ := payload["error"].(map[string]any)["details"].(map[string]any)
			if details["field"] != tc.wantField {
				t.Fatalf("field = %v, want %q", details["field"], tc.wantField)
			}
			if provider.prepareCalls != 0 {
				t.Fatalf("prepare calls = %d, want no platform call", provider.prepareCalls)
			}
		})
	}
}

// TestPrepareStreamUsesStoredBroadcastSettingsOnly: 저장된 설정이 준비 옵션의
// 단일 출처다 — 요청 바디에 실린 방송 속성은 무시돼야 한다(#142).
func TestPrepareStreamUsesStoredBroadcastSettingsOnly(t *testing.T) {
	provider := &stubStreamingProvider{prepared: streaming.PreparedBroadcast{
		Provider:  auth.StreamingProviderYouTube,
		IngestURL: "rtmps://a.rtmps.youtube.com/live2/secret",
	}}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)

	thumbnail := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	if response, payload := putBroadcast(t, server.URL, created.SessionID, ownerToken,
		`{"title":"저장 제목","description":"저장 설명","privacy":"private","made_for_kids":true,"category_id":"22","thumbnail":{"mime":"image/png","data_base64":"`+thumbnail+`"}}`); response.StatusCode != http.StatusOK {
		t.Fatalf("put broadcast status = %d (payload %v)", response.StatusCode, payload)
	}

	// 준비 요청 바디에는 방송 속성이 없다 — 옛 오버라이드 필드를 보내면
	// 조용히 무시하지 않고 400으로 거절한다.
	if response, _ := prepareStream(t, server.URL, created.SessionID, ownerToken,
		`{"title":"이번만","privacy":"public","made_for_kids":false}`); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for removed override fields", response.StatusCode)
	}
	if provider.prepareCalls != 0 {
		t.Fatalf("prepare calls = %d, want no platform call", provider.prepareCalls)
	}

	// 저장값이 그대로 준비 옵션이 된다(트랙이 없어 409로 끝나지만 Prepare는
	// 이미 호출된 뒤다).
	prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	options := provider.lastOptions
	if options.Title != "저장 제목" || options.Description != "저장 설명" || options.Privacy != "private" ||
		options.CategoryID != "22" || options.MadeForKids == nil || !*options.MadeForKids {
		t.Fatalf("options = %+v, want stored settings only", options)
	}
	if options.Thumbnail == nil || string(options.Thumbnail.Data) != "png-bytes" || options.Thumbnail.MIME != "image/png" {
		t.Fatalf("thumbnail option = %+v", options.Thumbnail)
	}
}

// TestPrepareStreamMadeForKidsComesFromStoredSettings: 저장값에 아동용 여부가
// 없으면 플랫폼 호출 전에 400이고, 저장하면 통과해야 한다.
func TestPrepareStreamMadeForKidsComesFromStoredSettings(t *testing.T) {
	provider := &stubStreamingProvider{prepareErr: auth.ErrStreamingNotConnected}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)

	response, _ := prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	if response.StatusCode != http.StatusBadRequest || provider.prepareCalls != 0 {
		t.Fatalf("status = %d, prepare calls = %d, want 400 without platform call", response.StatusCode, provider.prepareCalls)
	}

	putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":true}`)
	response, _ = prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	if response.StatusCode != http.StatusConflict || provider.prepareCalls != 1 {
		t.Fatalf("status = %d, prepare calls = %d, want the stored value to satisfy the requirement", response.StatusCode, provider.prepareCalls)
	}
}

// TestGoLiveWithoutPrepare: 준비되지 않은 세션의 라이브 전환은 409다.
func TestGoLiveWithoutPrepare(t *testing.T) {
	provider := &stubStreamingProvider{}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)

	response, payload := goLive(t, server.URL, created.SessionID, ownerToken)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (payload %v)", response.StatusCode, payload)
	}
	if streamErrorCode(payload) != "broadcast_not_prepared" {
		t.Fatalf("error code = %q, want broadcast_not_prepared", streamErrorCode(payload))
	}
	if provider.goLiveCalls != 0 {
		t.Fatalf("go live calls = %d, want no platform call", provider.goLiveCalls)
	}
}

// TestPutBroadcastRejectedWhilePreparing: 플랫폼 왕복 중에 들어온 설정 변경은
// 거절해야 한다. 통과시키면 조회에는 새 설정이 보이지만 실제 YouTube 방송은
// 이전 설정으로 만들어져 둘이 갈린다(PR #146 리뷰).
func TestPutBroadcastRejectedWhilePreparing(t *testing.T) {
	provider := &stubStreamingProvider{
		prepared: streaming.PreparedBroadcast{
			Provider:  auth.StreamingProviderYouTube,
			IngestURL: "rtmps://a.rtmps.youtube.com/live2/secret",
		},
		prepareEntered: make(chan struct{}),
		prepareRelease: make(chan struct{}),
	}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)

	if response, payload := putBroadcast(t, server.URL, created.SessionID, ownerToken,
		`{"title":"설정 A","made_for_kids":false}`); response.StatusCode != http.StatusOK {
		t.Fatalf("put broadcast status = %d (payload %v)", response.StatusCode, payload)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	}()

	// 플랫폼 왕복 한가운데서 설정 변경을 시도한다. 단언이 실패해도 붙잡아 둔
	// 준비를 반드시 풀어줘야 테스트가 멈추지 않는다.
	<-provider.prepareEntered
	// 단언이 실패해도 붙잡아 둔 준비를 반드시 풀어줘야 테스트가 멈추지 않는다.
	release := sync.OnceFunc(func() {
		close(provider.prepareRelease)
		<-done
	})
	defer release()
	response, payload := putBroadcast(t, server.URL, created.SessionID, ownerToken,
		`{"title":"설정 B","made_for_kids":false}`)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while preparing (payload %v)", response.StatusCode, payload)
	}
	release()

	// 준비가 읽어간 설정과 저장값이 같아야 한다.
	if provider.lastOptions.Title != "설정 A" {
		t.Fatalf("prepare options title = %q, want 설정 A", provider.lastOptions.Title)
	}
	broadcast, _ := getSessionPayload(t, server.URL, created.SessionID, ownerToken)["broadcast"].(map[string]any)
	if broadcast["title"] != "설정 A" {
		t.Fatalf("stored title = %v, want 설정 A", broadcast["title"])
	}
}

// TestPrepareRejectedWhilePreparing: 준비 요청이 겹치면 뒤엣것은 플랫폼을
// 부르지 않고 409여야 한다 — 방송이 둘 만들어지면 하나는 고아가 된다.
func TestPrepareRejectedWhilePreparing(t *testing.T) {
	provider := &stubStreamingProvider{
		prepared: streaming.PreparedBroadcast{
			Provider:  auth.StreamingProviderYouTube,
			IngestURL: "rtmps://a.rtmps.youtube.com/live2/secret",
		},
		prepareEntered: make(chan struct{}),
		prepareRelease: make(chan struct{}),
	}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)
	putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`)

	done := make(chan struct{})
	go func() {
		defer close(done)
		prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	}()

	<-provider.prepareEntered
	release := sync.OnceFunc(func() {
		close(provider.prepareRelease)
		<-done
	})
	defer release()
	response, payload := prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for the second prepare (payload %v)", response.StatusCode, payload)
	}
	if streamErrorCode(payload) != "broadcast_prepared" {
		t.Fatalf("error code = %q, want broadcast_prepared", streamErrorCode(payload))
	}
	release()
	if provider.prepareCalls != 1 {
		t.Fatalf("prepare calls = %d, want 1", provider.prepareCalls)
	}
}

// TestSessionEndDiscardsPreparedBroadcast: 세션이 어떤 이유로 끝나든 라이브까지
// 가지 못한 방송은 플랫폼에서 지워야 한다(PR #146 리뷰). 정리 훅이 서버 조립
// 시점에 실제로 걸려 있는지까지 확인한다.
func TestSessionEndDiscardsPreparedBroadcast(t *testing.T) {
	provider := &stubStreamingProvider{}
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
	})
	created, _ := createTestSession(t, server.URL, nil)

	if _, err := manager.BeginBroadcastPrepare(created.SessionID); err != nil {
		t.Fatal(err)
	}
	broadcast := session.PlatformBroadcast{Provider: string(auth.StreamingProviderYouTube), BroadcastID: "bid-1"}
	if _, err := manager.MarkBroadcastPrepared(created.SessionID, broadcast); err != nil {
		t.Fatal(err)
	}

	// WebRTC 실패·유예 시간 초과·로그아웃이 모두 이 경로로 모인다.
	if err := manager.Delete(created.SessionID, "peer_connection_failed"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	calls, stopped := provider.stopped()
	for time.Now().Before(deadline) && calls == 0 {
		time.Sleep(5 * time.Millisecond)
		calls, stopped = provider.stopped()
	}
	if calls != 1 || stopped.BroadcastID != "bid-1" {
		t.Fatalf("stop calls = %d, stopped = %+v, want the prepared broadcast discarded", calls, stopped)
	}
}

// TestSessionEndDuringGoLiveEndsTheLiveBroadcast: 라이브 전환 왕복 중에 세션이
// 끝나면 중지가 이긴다 — 이미 나간 전환 요청은 취소할 수 없으므로, 전환 결과를
// 받은 서버가 방송을 즉시 종료시켜야 한다. autoStop(약 1분)에 맡기면 그동안
// 시청자에게 노출된다(PR #146 리뷰).
func TestSessionEndDuringGoLiveEndsTheLiveBroadcast(t *testing.T) {
	provider := &stubStreamingProvider{
		goLiveEntered: make(chan struct{}),
		goLiveRelease: make(chan struct{}),
	}
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)

	if _, err := manager.BeginBroadcastPrepare(created.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.MarkBroadcastPrepared(created.SessionID, session.PlatformBroadcast{
		Provider:    string(auth.StreamingProviderYouTube),
		BroadcastID: "bid-1",
	}); err != nil {
		t.Fatal(err)
	}

	var goLiveStatus int
	var goLiveCode string
	done := make(chan struct{})
	go func() {
		defer close(done)
		response, payload := goLive(t, server.URL, created.SessionID, ownerToken)
		goLiveStatus = response.StatusCode
		goLiveCode = streamErrorCode(payload)
	}()

	// 전환 왕복 한가운데서 세션을 끝낸다.
	<-provider.goLiveEntered
	release := sync.OnceFunc(func() {
		close(provider.goLiveRelease)
		<-done
	})
	defer release()
	if err := manager.Delete(created.SessionID, "peer_connection_failed"); err != nil {
		t.Fatal(err)
	}
	release()

	if goLiveStatus != http.StatusConflict || goLiveCode != "broadcast_stopped" {
		t.Fatalf("go live = %d %q, want 409 broadcast_stopped", goLiveStatus, goLiveCode)
	}
	calls, ended := provider.ended()
	if calls != 1 || ended.BroadcastID != "bid-1" {
		t.Fatalf("end live calls = %d, ended = %+v, want the live broadcast ended immediately", calls, ended)
	}
	// 라이브가 된 방송은 삭제가 아니라 종료 대상이다.
	if stopCalls, _ := provider.stopped(); stopCalls != 0 {
		t.Fatalf("stop calls = %d, want the broadcast ended, not deleted", stopCalls)
	}
}

// TestLegacyStartStreamKeepsPreviousBehaviour: 제거 예정인 stream/start는
// 클라이언트가 새 API로 옮겨갈 때까지 종전 계약 그대로 남는다(PR #146 리뷰).
// 저장 설정 위에 바디가 덮어쓰고, autoStart를 켜 플랫폼이 알아서 라이브로
// 넘기며, 새 경로의 방송 단계는 건드리지 않는다.
func TestLegacyStartStreamKeepsPreviousBehaviour(t *testing.T) {
	provider := &stubStreamingProvider{prepared: streaming.PreparedBroadcast{
		Provider:    auth.StreamingProviderYouTube,
		IngestURL:   "rtmps://a.rtmps.youtube.com/live2/secret",
		BroadcastID: "legacy-1",
	}}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)
	putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"title":"저장 제목","made_for_kids":false}`)

	// 트랙이 없어 409로 끝나지만 Prepare는 이미 호출된 뒤다.
	response, payload := postStream(t, server.URL, created.SessionID, ownerToken, "start",
		`{"provider":"youtube","title":"이번만","privacy":"public","made_for_kids":true}`)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 without a video track (payload %v)", response.StatusCode, payload)
	}
	options := provider.lastOptions
	if !options.AutoStart {
		t.Fatal("legacy start must keep autoStart on")
	}
	// 바디 오버라이드가 종전처럼 살아 있어야 한다.
	if options.Title != "이번만" || options.Privacy != "public" || options.MadeForKids == nil || !*options.MadeForKids {
		t.Fatalf("options = %+v, want the request body override", options)
	}
	// 단계를 기록하지 않으므로 설정 변경도 계속 열려 있다.
	stream, _ := getSessionPayload(t, server.URL, created.SessionID, ownerToken)["stream"].(map[string]any)
	if stream["broadcast_phase"] != string(session.BroadcastPhaseIdle) {
		t.Fatalf("broadcast_phase = %v, want idle on the legacy path", stream["broadcast_phase"])
	}
	if response, _ := putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`); response.StatusCode != http.StatusOK {
		t.Fatalf("put broadcast after legacy start = %d, want 200", response.StatusCode)
	}
}

// TestLegacyStartStreamRejectedAfterPrepare: 새 경로가 이미 방송을 잡고 있으면
// 종전 경로를 열어두지 않는다 — 방송이 둘 생기면 하나는 고아가 된다.
func TestLegacyStartStreamRejectedAfterPrepare(t *testing.T) {
	provider := &stubStreamingProvider{}
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)
	putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`)
	if _, err := manager.BeginBroadcastPrepare(created.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.MarkBroadcastPrepared(created.SessionID, session.PlatformBroadcast{
		Provider:    string(auth.StreamingProviderYouTube),
		BroadcastID: "bid-1",
	}); err != nil {
		t.Fatal(err)
	}

	response, payload := postStream(t, server.URL, created.SessionID, ownerToken, "start", `{"made_for_kids":false}`)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (payload %v)", response.StatusCode, payload)
	}
	if streamErrorCode(payload) != "broadcast_prepared" {
		t.Fatalf("error code = %q, want broadcast_prepared", streamErrorCode(payload))
	}
	if provider.prepareCalls != 0 {
		t.Fatalf("prepare calls = %d, want no platform call", provider.prepareCalls)
	}
}

func getBroadcastDefaults(t *testing.T, baseURL, sessionID, ownerToken string) (*http.Response, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/sessions/"+sessionID+"/broadcast/defaults", nil)
	if err != nil {
		t.Fatal(err)
	}
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

// TestGetBroadcastDefaultsReturnsPreviousBroadcast: 직전 방송이 있는 계정은
// 그 값이 그대로 폼 초기값으로 나와야 한다(#143).
func TestGetBroadcastDefaultsReturnsPreviousBroadcast(t *testing.T) {
	madeForKids := false
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: &stubStreamingProvider{defaults: streaming.BroadcastDefaults{
			Title:       "직전 방송",
			Description: "직전 설명",
			Privacy:     "public",
			MadeForKids: &madeForKids,
			CategoryID:  "20",
		}},
	})
	created, ownerToken := createTestSession(t, server.URL, nil)

	response, payload := getBroadcastDefaults(t, server.URL, created.SessionID, ownerToken)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (payload %v)", response.StatusCode, payload)
	}
	if payload["title"] != "직전 방송" || payload["description"] != "직전 설명" ||
		payload["privacy"] != "public" || payload["category_id"] != "20" ||
		payload["made_for_kids"] != false {
		t.Fatalf("defaults = %v", payload)
	}
}

// TestGetBroadcastDefaultsFallsBackOnLookupFailure: 조회에 실패해도 폼은 열려야
// 하므로 200에 폴백값이 나와야 한다. 아동용 여부는 미선택(null)이다.
func TestGetBroadcastDefaultsFallsBackOnLookupFailure(t *testing.T) {
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: &stubStreamingProvider{defaultsErr: auth.ErrStreamingNotConnected},
	})
	created, ownerToken := createTestSession(t, server.URL, nil)

	response, payload := getBroadcastDefaults(t, server.URL, created.SessionID, ownerToken)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (payload %v)", response.StatusCode, payload)
	}
	if payload["title"] != "InnoLive 방송" || payload["privacy"] != "unlisted" ||
		payload["category_id"] != "" || payload["made_for_kids"] != nil {
		t.Fatalf("defaults = %v, want fallback", payload)
	}
}

// TestGetBroadcastDefaultsFallsBackWithoutProvider: 플랫폼 송출이 조립되지 않은
// 배포(자격증명 미설정·벤치)에서도 폼은 폴백값으로 열린다.
func TestGetBroadcastDefaultsFallsBackWithoutProvider(t *testing.T) {
	server := newStreamTestApplication(t, nil)
	created, ownerToken := createTestSession(t, server.URL, nil)

	response, payload := getBroadcastDefaults(t, server.URL, created.SessionID, ownerToken)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (payload %v)", response.StatusCode, payload)
	}
	if payload["title"] != "InnoLive 방송" || payload["privacy"] != "unlisted" {
		t.Fatalf("defaults = %v, want fallback", payload)
	}
}
