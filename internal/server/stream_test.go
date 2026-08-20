package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/origin"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"
)

type stubStreamingProvider struct {
	prepared     streaming.PreparedBroadcast
	prepareErr   error
	prepareCalls int
	lastOptions  streaming.PrepareOptions
}

func (s *stubStreamingProvider) Prepare(_ context.Context, _ uuid.UUID, options streaming.PrepareOptions) (streaming.PreparedBroadcast, error) {
	s.prepareCalls++
	s.lastOptions = options
	return s.prepared, s.prepareErr
}

func (s *stubStreamingProvider) Stop(context.Context, uuid.UUID, streaming.PreparedBroadcast) error {
	return nil
}

func newStreamTestApplication(t *testing.T, providers map[auth.StreamingProvider]streaming.Provider) *httptest.Server {
	t.Helper()
	cfg := config.Config{
		HTTPAddr:                ":0",
		PrivacyMode:             config.PrivacyModeBypass,
		PrivacyFixedDelay:       time.Millisecond,
		AITimeout:               time.Second,
		FFmpegPath:              "ffmpeg",
		UDPPortMin:              41200,
		UDPPortMax:              41300,
		DisconnectedGracePeriod: 100 * time.Millisecond,
		FrameQueueSize:          2,
		RequireSessionAuth:      true,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := metrics.New()
	manager, err := session.NewManager(cfg, logger, registry, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	origins, err := origin.NewConfig(false, []string{"https://client.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(cfg, logger, registry, manager, nil, origins, nil, providers).Handler())
	t.Cleanup(server.Close)
	return server
}

func startStream(t *testing.T, baseURL, sessionID, ownerToken, body string) (*http.Response, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, baseURL+"/sessions/"+sessionID+"/stream/start", bytes.NewBufferString(body))
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

func streamErrorCode(payload map[string]any) string {
	errorBody, _ := payload["error"].(map[string]any)
	code, _ := errorBody["code"].(string)
	return code
}

// TestStartStreamNotConfigured: 플랫폼 송출이 조립되지 않은 배포(자격증명
// 미설정·벤치)에서는 종전 계약(501)이 유지돼야 한다.
func TestStartStreamNotConfigured(t *testing.T) {
	server := newStreamTestApplication(t, nil)
	created, ownerToken := createTestSession(t, server.URL, nil)

	response, payload := startStream(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`)
	if response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", response.StatusCode)
	}
	if streamErrorCode(payload) != "not_supported" {
		t.Fatalf("error code = %q", streamErrorCode(payload))
	}
}

func TestStartStreamMapsProviderErrors(t *testing.T) {
	cases := []struct {
		name       string
		provider   *stubStreamingProvider
		wantStatus int
		wantCode   string
	}{
		{
			name:       "not connected",
			provider:   &stubStreamingProvider{prepareErr: auth.ErrStreamingNotConnected},
			wantStatus: http.StatusConflict,
			wantCode:   "streaming_not_connected",
		},
		{
			name:       "live blocked",
			provider:   &stubStreamingProvider{prepareErr: streaming.ErrLiveStreamingBlocked},
			wantStatus: http.StatusForbidden,
			wantCode:   "live_streaming_blocked",
		},
		{
			// 무효 refresh token은 재시도가 아니라 재연결 안내여야 한다(#88).
			name:       "reconnect required",
			provider:   &stubStreamingProvider{prepareErr: auth.ErrStreamingReconnectRequired},
			wantStatus: http.StatusConflict,
			wantCode:   "streaming_reconnect_required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
				auth.StreamingProviderYouTube: tc.provider,
			})
			created, ownerToken := createTestSession(t, server.URL, nil)
			response, payload := startStream(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`)
			if response.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d (payload %v)", response.StatusCode, tc.wantStatus, payload)
			}
			if streamErrorCode(payload) != tc.wantCode {
				t.Fatalf("error code = %q, want %q", streamErrorCode(payload), tc.wantCode)
			}
			if tc.wantCode == "live_streaming_blocked" {
				details, _ := payload["error"].(map[string]any)["details"].(map[string]any)
				if details["help_url"] != streaming.LiveStreamingHelpURL {
					t.Fatalf("help_url = %v, want extendedHelp URL", details["help_url"])
				}
			}
		})
	}
}

// TestStartStreamRequiresTrackAfterPrepare: Prepare가 성공해도 비디오 트랙이
// 없으면 종전 409 계약을 유지해야 한다.
func TestStartStreamRequiresTrackAfterPrepare(t *testing.T) {
	provider := &stubStreamingProvider{prepared: streaming.PreparedBroadcast{
		Provider:  auth.StreamingProviderYouTube,
		IngestURL: "rtmps://a.rtmps.youtube.com/live2/secret",
	}}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)

	response, payload := startStream(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (payload %v)", response.StatusCode, payload)
	}
	if streamErrorCode(payload) != "conflict" {
		t.Fatalf("error code = %q, want conflict", streamErrorCode(payload))
	}
}

func TestStartStreamRejectsUnknownProvider(t *testing.T) {
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: &stubStreamingProvider{},
	})
	created, ownerToken := createTestSession(t, server.URL, nil)

	response, payload := startStream(t, server.URL, created.SessionID, ownerToken, `{"provider":"soop","made_for_kids":false}`)
	if response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 for unregistered provider", response.StatusCode)
	}
	if streamErrorCode(payload) != "not_supported" {
		t.Fatalf("error code = %q", streamErrorCode(payload))
	}
}

// TestStartStreamRequiresMadeForKids: 시청자층 신고는 사용자가 직접 골라야
// 하므로, 미선택 요청은 프로바이더 호출 전에 400으로 거절해야 한다.
func TestStartStreamRequiresMadeForKids(t *testing.T) {
	provider := &stubStreamingProvider{prepareErr: auth.ErrStreamingNotConnected}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{
		auth.StreamingProviderYouTube: provider,
	})
	created, ownerToken := createTestSession(t, server.URL, nil)

	response, payload := startStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (payload %v)", response.StatusCode, payload)
	}
	if provider.prepareCalls != 0 {
		t.Fatalf("prepare calls = %d, want 0", provider.prepareCalls)
	}
}

func TestStopStreamNotActive(t *testing.T) {
	server := newStreamTestApplication(t, nil)
	created, ownerToken := createTestSession(t, server.URL, nil)

	request, err := http.NewRequest(http.MethodPost, server.URL+"/sessions/"+created.SessionID+"/stream/stop", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Session-Owner-Token", ownerToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", response.StatusCode)
	}
	payload := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&payload)
	if streamErrorCode(payload) != "stream_not_active" {
		t.Fatalf("error code = %q, want stream_not_active", streamErrorCode(payload))
	}
}

func TestPauseAndResumeStreamNotActive(t *testing.T) {
	server := newStreamTestApplication(t, nil)
	created, ownerToken := createTestSession(t, server.URL, nil)

	for _, operation := range []string{"pause", "resume"} {
		t.Run(operation, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, server.URL+"/sessions/"+created.SessionID+"/stream/"+operation, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-Session-Owner-Token", ownerToken)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusConflict {
				t.Fatalf("status = %d, want 409", response.StatusCode)
			}
			payload := map[string]any{}
			_ = json.NewDecoder(response.Body).Decode(&payload)
			if streamErrorCode(payload) != "stream_not_active" {
				t.Fatalf("error code = %q, want stream_not_active", streamErrorCode(payload))
			}
		})
	}
}

func TestPatchAnonymizationIsIndependentFromBroadcastState(t *testing.T) {
	server := newStreamTestApplication(t, nil)
	created, ownerToken := createTestSession(t, server.URL, nil)

	for _, enabled := range []bool{true, false} {
		body, err := json.Marshal(map[string]bool{"enabled": enabled})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest(http.MethodPatch, server.URL+"/sessions/"+created.SessionID+"/anonymization", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Session-Owner-Token", ownerToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		payload := map[string]any{}
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (payload %v)", response.StatusCode, payload)
		}
		media, _ := payload["media"].(map[string]any)
		if got, ok := media["anonymization_enabled"].(bool); !ok || got != enabled {
			t.Fatalf("anonymization_enabled = %v, want %v", media["anonymization_enabled"], enabled)
		}
		stream, _ := payload["stream"].(map[string]any)
		if stream["status"] != "idle" {
			t.Fatalf("stream status = %v, want unchanged idle", stream["status"])
		}
	}
}

func TestPatchAnonymizationRequiresEnabled(t *testing.T) {
	server := newStreamTestApplication(t, nil)
	created, ownerToken := createTestSession(t, server.URL, nil)
	request, err := http.NewRequest(http.MethodPatch, server.URL+"/sessions/"+created.SessionID+"/anonymization", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Session-Owner-Token", ownerToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
}
