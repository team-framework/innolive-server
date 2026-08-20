package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
