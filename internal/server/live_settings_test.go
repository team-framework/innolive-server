package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
)

func (p *stubStreamingProvider) UpdateLive(_ context.Context, _ uuid.UUID, _ streaming.PreparedBroadcast, update streaming.LiveUpdate) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.liveUpdates = append(p.liveUpdates, update)
	return p.liveUpdateErr
}

type liveSettingsFixture struct {
	server     *httptest.Server
	manager    *session.Manager
	live       *session.Session
	ownerToken string
	youtube    *stubStreamingProvider
}

// newLiveSettingsFixture는 유튜브 방송이 준비된 세션을 만든다. 플랫폼 호출 없이
// 준비 단계로 옮긴다 — 방송 중 설정 변경은 준비·라이브 단계에서만 받는다.
func newLiveSettingsFixture(t *testing.T, prepared bool) liveSettingsFixture {
	t.Helper()
	youtube := &stubStreamingProvider{}
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{auth.StreamingProviderYouTube: youtube})
	live, ownerToken, err := manager.CreateForUser(uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	putBroadcast(t, server.URL, live.ID, ownerToken, `{"title":"처음 제목","made_for_kids":false,"category_id":"20"}`)
	if prepared {
		if _, err := manager.BeginBroadcastPrepare(live.ID, "youtube"); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.MarkBroadcastPrepared(live.ID, session.PlatformBroadcast{Provider: "youtube", BroadcastID: "bid-1"}, "youtube"); err != nil {
			t.Fatal(err)
		}
	}
	return liveSettingsFixture{server: server, manager: manager, live: live, ownerToken: ownerToken, youtube: youtube}
}

func (f liveSettingsFixture) patch(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPatch, f.server.URL+"/sessions/"+f.live.ID+"/broadcast/live?provider=youtube", bytes.NewBufferString(body))
	request.Header.Set("X-Session-Owner-Token", f.ownerToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&payload)
	return response.StatusCode, payload
}

func TestPatchLiveBroadcastUpdatesPlatformThenSettings(t *testing.T) {
	fixture := newLiveSettingsFixture(t, true)
	status, payload := fixture.patch(t, `{"title":"  새 제목  "}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d %v", status, payload)
	}
	if len(fixture.youtube.liveUpdates) != 1 || *fixture.youtube.liveUpdates[0].Title != "  새 제목  " || fixture.youtube.liveUpdates[0].Description != nil {
		t.Fatalf("platform updates = %+v, want only the title", fixture.youtube.liveUpdates)
	}
	settings := fixture.live.BroadcastSettings()
	if settings.Title != "새 제목" || settings.CategoryID != "20" {
		t.Fatalf("saved settings = %+v, want new title and kept category", settings)
	}
}

func TestPatchLiveBroadcastRejections(t *testing.T) {
	t.Run("not prepared or live", func(t *testing.T) {
		fixture := newLiveSettingsFixture(t, false)
		if status, payload := fixture.patch(t, `{"title":"새 제목"}`); status != http.StatusConflict || errorCode(payload) != "broadcast_not_live" {
			t.Fatalf("= %d %v, want 409 broadcast_not_live", status, payload)
		}
		if len(fixture.youtube.liveUpdates) != 0 {
			t.Fatal("platform must not be called")
		}
	})
	t.Run("field not changeable live", func(t *testing.T) {
		fixture := newLiveSettingsFixture(t, true)
		status, payload := fixture.patch(t, `{"title":"새 제목","privacy":"public","made_for_kids":true}`)
		if status != http.StatusBadRequest || errorCode(payload) != "field_not_changeable_live" {
			t.Fatalf("= %d %v, want 400 field_not_changeable_live", status, payload)
		}
		if len(fixture.youtube.liveUpdates) != 0 {
			t.Fatal("platform must not be called")
		}
	})
	t.Run("invalid title", func(t *testing.T) {
		fixture := newLiveSettingsFixture(t, true)
		if status, payload := fixture.patch(t, `{"title":"<방송>"}`); status != http.StatusBadRequest {
			t.Fatalf("= %d %v, want 400", status, payload)
		}
	})
	t.Run("platform failure keeps saved settings", func(t *testing.T) {
		fixture := newLiveSettingsFixture(t, true)
		fixture.youtube.liveUpdateErr = errors.New("videos.update 500")
		if status, payload := fixture.patch(t, `{"title":"새 제목"}`); status != http.StatusBadGateway || errorCode(payload) != "streaming_update_failed" {
			t.Fatalf("= %d %v, want 502 streaming_update_failed", status, payload)
		}
		if title := fixture.live.BroadcastSettings().Title; title != "처음 제목" {
			t.Fatalf("saved title = %q, want unchanged after platform failure", title)
		}
	})
}
