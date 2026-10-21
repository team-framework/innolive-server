package server

import (
	"net/http"
	"strings"
	"testing"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
)

const faceSwapNoticePrefix = session.FaceSwapDescriptionNotice + "\n\n"

// 얼굴 합성 세션의 유튜브 방송 준비는 설명 앞에 합성 안내문을 붙여 보내고,
// 저장값은 사용자 입력 그대로 둔다.
func TestPrepareStreamAddsFaceSwapNoticeToDescription(t *testing.T) {
	provider := &stubStreamingProvider{prepared: streaming.PreparedBroadcast{
		Provider:  auth.StreamingProviderYouTube,
		IngestURL: "rtmps://a.rtmps.youtube.com/live2/secret",
	}}
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
	})
	live, ownerToken, err := manager.CreateForUser(uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// 합성 세션 생성은 합성 워커가 있어야 해 필드를 직접 바꾼다(생성 뒤 불변 값).
	live.FaceAnonymization = session.FaceAnonymizationFaceSwap
	if response, payload := putBroadcast(t, server.URL, live.ID, ownerToken, `{"description":"저장 설명","made_for_kids":false}`); response.StatusCode != http.StatusOK {
		t.Fatalf("put broadcast status = %d (payload %v)", response.StatusCode, payload)
	}

	prepareStream(t, server.URL, live.ID, ownerToken, `{}`)
	if provider.prepareCalls != 1 || provider.lastOptions.Description != faceSwapNoticePrefix+"저장 설명" {
		t.Fatalf("prepare calls = %d, description = %q, want notice + stored description", provider.prepareCalls, provider.lastOptions.Description)
	}
	if stored := live.BroadcastSettings().Description; stored != "저장 설명" {
		t.Fatalf("stored description = %q, want user input unchanged", stored)
	}
}

// 안내문 자리를 남기지 않은 긴 설명은 합성 세션에서 저장 시점에 400이다.
func TestPutBroadcastRejectsDescriptionWithoutRoomForFaceSwapNotice(t *testing.T) {
	server, manager := newStreamTestApplicationWithManager(t, nil)
	live, ownerToken, err := manager.CreateForUser(uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	live.FaceAnonymization = session.FaceAnonymizationFaceSwap
	body := `{"description":"` + strings.Repeat("가", session.MaxYouTubeDescriptionLength) + `"}`
	response, payload := putBroadcast(t, server.URL, live.ID, ownerToken, body)
	details, _ := payload["error"].(map[string]any)["details"].(map[string]any)
	if response.StatusCode != http.StatusBadRequest || details["field"] != "description" {
		t.Fatalf("status = %d payload = %v, want 400 description", response.StatusCode, payload)
	}
}

// 방송 중 설명을 바꿔도 플랫폼에는 안내문이 붙어 나가고 저장값은 입력 그대로다.
func TestPatchLiveBroadcastKeepsFaceSwapNotice(t *testing.T) {
	fixture := newLiveSettingsFixture(t, true)
	fixture.live.FaceAnonymization = session.FaceAnonymizationFaceSwap
	status, payload := fixture.patch(t, `{"description":"새 설명"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d %v", status, payload)
	}
	if len(fixture.youtube.liveUpdates) != 1 || fixture.youtube.liveUpdates[0].Description == nil ||
		*fixture.youtube.liveUpdates[0].Description != faceSwapNoticePrefix+"새 설명" {
		t.Fatalf("platform updates = %+v, want notice + new description", fixture.youtube.liveUpdates)
	}
	if stored := fixture.live.BroadcastSettings().Description; stored != "새 설명" {
		t.Fatalf("stored description = %q, want user input", stored)
	}

	// 제목만 바꾸면 설명은 건드리지 않는다 — 플랫폼의 현재 설명(안내문 포함)이 유지된다.
	if status, payload := fixture.patch(t, `{"title":"새 제목"}`); status != http.StatusOK || fixture.youtube.liveUpdates[1].Description != nil {
		t.Fatalf("status = %d %v, updates = %+v, want title only", status, payload, fixture.youtube.liveUpdates)
	}
}

// 직전 방송이 합성 방송이었으면 폼 초기값에서 안내문을 뗀다 — 블러 방송에 남거나
// 합성 방송에 두 번 붙지 않게 한다.
func TestGetBroadcastDefaultsStripsFaceSwapNotice(t *testing.T) {
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: &stubStreamingProvider{defaults: streaming.BroadcastDefaults{
			Title:       "직전 방송",
			Description: faceSwapNoticePrefix + "직전 설명",
		}},
	})
	created, ownerToken := createTestSession(t, server.URL, nil)
	response, payload := getBroadcastDefaults(t, server.URL, created.SessionID, ownerToken)
	if response.StatusCode != http.StatusOK || payload["description"] != "직전 설명" {
		t.Fatalf("status = %d defaults = %v, want notice stripped", response.StatusCode, payload)
	}
}
