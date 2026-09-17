package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"
)

// chzzkTestSession은 치지직 세션 하나를 만들고 owner token을 돌려준다.
func chzzkTestSession(t *testing.T, baseURL string) (session.Response, string) {
	t.Helper()
	response, payload := createSessionWithProvider(t, baseURL, string(auth.StreamingProviderChzzk))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create session status = %d, payload %v", response.StatusCode, payload)
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
	return created.Response, created.OwnerToken
}

func chzzkTestApplication(t *testing.T) (*stubStreamingProvider, string) {
	t.Helper()
	chzzk := &stubStreamingProvider{}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: &stubStreamingProvider{},
		auth.StreamingProviderChzzk:   chzzk,
	})
	return chzzk, server.URL
}

// TestChzzkBroadcastSettingsRoundTrip: 정상 저장 경로 — 저장한 값이 조회
// 응답의 chzzk_broadcast로 돌아온다.
func TestChzzkBroadcastSettingsRoundTrip(t *testing.T) {
	_, baseURL := chzzkTestApplication(t)
	created, ownerToken := chzzkTestSession(t, baseURL)

	response, payload := putBroadcast(t, baseURL, created.SessionID, ownerToken,
		`{"title":"치지직 방송","category_type":"GAME","category_id":"GTA5","tags":["게임","retro2"]}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, payload %v", response.StatusCode, payload)
	}
	broadcast, _ := payload["chzzk_broadcast"].(map[string]any)
	if broadcast == nil {
		t.Fatalf("chzzk_broadcast is missing: %v", payload)
	}
	if broadcast["title"] != "치지직 방송" || broadcast["category_type"] != "GAME" || broadcast["category_id"] != "GTA5" {
		t.Fatalf("chzzk_broadcast = %v", broadcast)
	}
	tags, _ := broadcast["tags"].([]any)
	if len(tags) != 2 || tags[0] != "게임" {
		t.Fatalf("tags = %v", tags)
	}
	// 유튜브 설정 자리는 비어 있어야 한다 — 모델이 분리돼 있다는 뜻이다.
	if payload["broadcast"] != nil {
		t.Fatalf("youtube broadcast must stay empty on a chzzk session: %v", payload["broadcast"])
	}
}

// TestChzzkBroadcastRejectsUnknownCategoryType: 열거값 밖은 400이고 어떤
// 필드인지 알려준다.
func TestChzzkBroadcastRejectsUnknownCategoryType(t *testing.T) {
	_, baseURL := chzzkTestApplication(t)
	created, ownerToken := chzzkTestSession(t, baseURL)

	response, payload := putBroadcast(t, baseURL, created.SessionID, ownerToken,
		`{"category_type":"MUSIC","category_id":"x"}`)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (payload %v)", response.StatusCode, payload)
	}
	details, _ := payload["error"].(map[string]any)["details"].(map[string]any)
	if details["field"] != "category_type" {
		t.Fatalf("details = %v, want category_type", details)
	}
}

// TestChzzkBroadcastRejectsInvalidTags: 공백·특수문자 태그는 조용히 다듬지
// 않고 거절한다 — 저장했다고 믿은 태그와 실제 방송이 달라지면 안 된다.
func TestChzzkBroadcastRejectsInvalidTags(t *testing.T) {
	cases := map[string]string{
		"whitespace":        `{"tags":["고전 명작"]}`,
		"special character": `{"tags":["retro!"]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, baseURL := chzzkTestApplication(t)
			created, ownerToken := chzzkTestSession(t, baseURL)
			response, payload := putBroadcast(t, baseURL, created.SessionID, ownerToken, body)
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (payload %v)", response.StatusCode, payload)
			}
			details, _ := payload["error"].(map[string]any)["details"].(map[string]any)
			if details["field"] != "tags[0]" {
				t.Fatalf("details = %v, want tags[0]", details)
			}
		})
	}
}

// TestBroadcastBodiesDoNotCrossPlatforms: 플랫폼이 다른 바디는 400이다.
// DisallowUnknownFields가 막아주는 동작이라 계약으로 고정해 둔다.
func TestBroadcastBodiesDoNotCrossPlatforms(t *testing.T) {
	t.Run("chzzk body on a youtube session", func(t *testing.T) {
		_, baseURL := chzzkTestApplication(t)
		created, ownerToken := createTestSession(t, baseURL, nil)
		if created.Provider != string(auth.StreamingProviderYouTube) {
			t.Fatalf("provider = %q", created.Provider)
		}
		response, _ := putBroadcast(t, baseURL, created.SessionID, ownerToken, `{"tags":["게임"]}`)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", response.StatusCode)
		}
	})
	t.Run("youtube body on a chzzk session", func(t *testing.T) {
		_, baseURL := chzzkTestApplication(t)
		created, ownerToken := chzzkTestSession(t, baseURL)
		response, _ := putBroadcast(t, baseURL, created.SessionID, ownerToken, `{"made_for_kids":true}`)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", response.StatusCode)
		}
	})
}

// TestChzzkPrepareDoesNotRequireMadeForKids: 아동용 신고는 유튜브 개념이다.
// 치지직 세션의 prepare가 그걸로 막히면 안 된다(#229 통과 기준).
func TestChzzkPrepareDoesNotRequireMadeForKids(t *testing.T) {
	chzzk, baseURL := chzzkTestApplication(t)
	created, ownerToken := chzzkTestSession(t, baseURL)
	putBroadcast(t, baseURL, created.SessionID, ownerToken,
		`{"title":"치지직 방송","category_type":"GAME","category_id":"GTA5","tags":["게임"]}`)

	// 트랙이 없어 409로 끝나지만, 그 전에 프로바이더까지 도달해야 한다.
	response, payload := prepareStream(t, baseURL, created.SessionID, ownerToken, `{}`)
	if response.StatusCode == http.StatusBadRequest {
		t.Fatalf("chzzk prepare must not be rejected for made_for_kids: %v", payload)
	}
	if chzzk.prepareCalls != 1 {
		t.Fatalf("chzzk prepare calls = %d, want 1", chzzk.prepareCalls)
	}
	if chzzk.lastOptions.Title != "치지직 방송" || chzzk.lastOptions.CategoryType != "GAME" ||
		chzzk.lastOptions.CategoryID != "GTA5" || len(chzzk.lastOptions.Tags) != 1 {
		t.Fatalf("prepare options = %+v, want the stored chzzk settings", chzzk.lastOptions)
	}
	if chzzk.lastOptions.MadeForKids != nil {
		t.Fatalf("made_for_kids = %v, want unset on chzzk", *chzzk.lastOptions.MadeForKids)
	}
}

// TestYouTubeMadeForKidsRejectionReleasesPreparation: 유튜브 거절이 준비
// 선점을 되돌려, 설정을 채운 뒤 곧바로 재시도할 수 있어야 한다(409로 막히면
// 사용자가 세션을 새로 만들어야 한다).
func TestYouTubeMadeForKidsRejectionReleasesPreparation(t *testing.T) {
	provider := &stubStreamingProvider{}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)

	response, _ := prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	if response.StatusCode != http.StatusBadRequest || provider.prepareCalls != 0 {
		t.Fatalf("status = %d, prepare calls = %d, want 400 without a platform call", response.StatusCode, provider.prepareCalls)
	}

	putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`)
	response, payload := prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	// 선점이 안 풀렸다면 BeginBroadcastPrepare가 막아 프로바이더까지 가지
	// 못한다. 즉 prepare 호출 1회가 곧 "선점이 풀렸다"의 증거다.
	if provider.prepareCalls != 1 {
		t.Fatalf("prepare calls = %d, want the retry to reach the platform (payload %v)", provider.prepareCalls, payload)
	}
	// 트랙이 없어 409로 끝나지만, 방송 단계 충돌이 아니라 트랙 부재여야 한다.
	if code := streamErrorCode(payload); code == "broadcast_preparing" || code == "broadcast_prepared" {
		t.Fatalf("error code = %q, want the preparation to have been released", code)
	}
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for the missing video track", response.StatusCode)
	}
}
