package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/streaming"
)

func TestGetYouTubeCategories(t *testing.T) {
	youtube := &stubStreamingProvider{categories: []streaming.VideoCategory{{ID: "20", Title: "게임"}}}
	server, _ := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{auth.StreamingProviderYouTube: youtube})

	response := mustRequest(t, http.MethodGet, server.URL+"/auth/youtube/categories", nil, nil)
	var got struct {
		Categories []streaming.VideoCategory `json:"categories"`
	}
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(got.Categories) != 1 || got.Categories[0].ID != "20" {
		t.Fatalf("status = %d categories = %+v", response.StatusCode, got.Categories)
	}

	youtube.categoriesErr = auth.ErrStreamingNotConnected
	response = mustRequest(t, http.MethodGet, server.URL+"/auth/youtube/categories", nil, nil)
	payload := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&payload)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict || streamErrorCode(payload) != "streaming_not_connected" {
		t.Fatalf("not connected = %d %q, want 409 streaming_not_connected", response.StatusCode, streamErrorCode(payload))
	}
}

// 유튜브 요청 한도는 일반 준비 실패(502)가 아니라 429로 알린다.
func TestPrepareReportsPlatformRateLimit(t *testing.T) {
	youtube := &stubStreamingProvider{prepareErr: streaming.ErrPlatformRateLimited}
	server, _ := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{auth.StreamingProviderYouTube: youtube})
	created, ownerToken := createTestSession(t, server.URL, nil)
	putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`)
	response, payload := prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	if response.StatusCode != http.StatusTooManyRequests || streamErrorCode(payload) != "streaming_rate_limited" {
		t.Fatalf("prepare = %d %q, want 429 streaming_rate_limited", response.StatusCode, streamErrorCode(payload))
	}
}
