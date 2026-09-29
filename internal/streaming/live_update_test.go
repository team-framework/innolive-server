package streaming

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// videos.update는 snippet을 통째 교체한다. 바꾸지 않는 항목(태그·언어·카테고리)은
// 현재 값 그대로 보내야 지워지지 않는다(#334).
func TestYouTubeUpdateLiveKeepsUntouchedSnippetFields(t *testing.T) {
	var sent map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /videos":
			if r.URL.Query().Get("id") != "bid-1" {
				t.Errorf("list id = %q", r.URL.Query().Get("id"))
			}
			_, _ = w.Write([]byte(`{"items":[{"snippet":{"title":"예전 제목","description":"설명","categoryId":"20",
				"tags":["게임"],"defaultLanguage":"ko","channelId":"ch","publishedAt":"2026-09-29T00:00:00Z","thumbnails":{},"liveBroadcastContent":"live"}}]}`))
		case "PUT /videos":
			_ = json.NewDecoder(r.Body).Decode(&sent)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	provider, err := NewYouTubeProvider(stubTokens{token: "at-value"}, newMemoryStore(), testCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	provider.apiBase = server.URL

	title := "  새 제목 "
	if err := provider.UpdateLive(context.Background(), uuid.New(), PreparedBroadcast{BroadcastID: "bid-1"}, LiveUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	snippet, _ := sent["snippet"].(map[string]any)
	if sent["id"] != "bid-1" || snippet["title"] != "새 제목" {
		t.Fatalf("sent = %v, want trimmed new title for bid-1", sent)
	}
	if snippet["description"] != "설명" || snippet["categoryId"] != "20" || snippet["defaultLanguage"] != "ko" {
		t.Fatalf("snippet = %v, want untouched fields kept", snippet)
	}
	if tags, _ := snippet["tags"].([]any); len(tags) != 1 || tags[0] != "게임" {
		t.Fatalf("tags = %v, want kept", snippet["tags"])
	}
	for _, readOnly := range []string{"channelId", "publishedAt", "thumbnails", "liveBroadcastContent"} {
		if _, ok := snippet[readOnly]; ok {
			t.Fatalf("read-only field %s was sent back", readOnly)
		}
	}
}

func TestYouTubeUpdateLiveFailsWhenVideoMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("must not update a missing video: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer server.Close()
	provider, err := NewYouTubeProvider(stubTokens{token: "at-value"}, newMemoryStore(), testCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	provider.apiBase = server.URL
	title := "제목"
	if err := provider.UpdateLive(context.Background(), uuid.New(), PreparedBroadcast{BroadcastID: "gone"}, LiveUpdate{Title: &title}); err == nil {
		t.Fatal("UpdateLive() = nil, want error for a missing video")
	}
}

func TestChzzkUpdateLivePatchesSetting(t *testing.T) {
	stub := &chzzkStub{t: t}
	provider := newChzzkProviderForTest(t, stub, stubTokens{token: "token-1"})
	title, categoryType, categoryID := "새 방송", "GAME", "League_of_Legends"
	err := provider.UpdateLive(context.Background(), uuid.New(), PreparedBroadcast{}, LiveUpdate{
		Title: &title, CategoryType: &categoryType, CategoryID: &categoryID, Tags: []string{"롤"}, TagsSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.requests) != 1 || stub.requests[0].Method != http.MethodPatch {
		t.Fatalf("requests = %d, want one PATCH", len(stub.requests))
	}
	body := stub.bodies[0]
	if body["defaultLiveTitle"] != "새 방송" || body["categoryType"] != "GAME" || body["categoryId"] != "League_of_Legends" {
		t.Fatalf("body = %v", body)
	}
}
