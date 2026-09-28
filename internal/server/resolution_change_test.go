package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/plan"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4/pkg/media"
)

// vp8KeyframeSize는 VP8 키프레임 헤더의 치수다. 키프레임이 아니면 ok가 false다.
func vp8KeyframeSize(frame []byte) (width, height int, ok bool) {
	if len(frame) < 10 || frame[0]&1 != 0 || frame[3] != 0x9d || frame[4] != 0x01 || frame[5] != 0x2a {
		return 0, 0, false
	}
	return int(frame[6]) | int(frame[7])<<8&0x3fff, int(frame[8]) | int(frame[9])<<8&0x3fff, true
}

// 해상도 변경(#283)은 같은 트랙으로 파이프라인을 새 디코더 핀으로 다시 띄운다.
// 미리보기(서버가 되돌려 보내는 처리 영상)의 치수가 바뀌는 것으로 확인한다.
func TestBroadcastResolutionChangeRestartsPipeline(t *testing.T) {
	cfg := testServerConfig()
	cfg.DecoderPinLongEdge = 320 // 720p 핀. FHD는 1920이다.
	application, manager := newTestApplicationWithConfig(t, cfg, nil)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	liveSession, ownerToken := createTestSession(t, httpServer.URL, nil)
	track, received := connectTestPublisher(t, httpServer.URL, liveSession.SessionID, ownerToken)

	frames := generateVP8Frames(t, 30)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ticker := time.NewTicker(time.Second / 30)
		defer ticker.Stop()
		for index := 0; ; index++ {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := track.WriteSample(media.Sample{Data: frames[index%len(frames)], Duration: time.Second / 30}); err != nil {
					return
				}
			}
		}
	}()
	waitForPreviewSize := func(width, height int) {
		t.Helper()
		deadline := time.After(20 * time.Second)
		for {
			select {
			case frame := <-received:
				if w, h, ok := vp8KeyframeSize(frame); ok && w == width && h == height {
					return
				}
			case <-deadline:
				t.Fatalf("no %dx%d preview keyframe", width, height)
			}
		}
	}
	waitForPreviewSize(320, 180)

	change := func(body string) *http.Response {
		return mustRequest(t, http.MethodPut, httpServer.URL+"/sessions/"+liveSession.SessionID+"/broadcast-resolution", strings.NewReader(body), bearer(ownerToken))
	}
	response := change(`{"resolution":"1080p"}`)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsupported resolution status = %d, want 400", response.StatusCode)
	}
	response = change(`{"resolution":"fhd"}`)
	var got struct {
		BroadcastResolution string `json:"broadcast_resolution"`
	}
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || got.BroadcastResolution != "fhd" {
		t.Fatalf("change status = %d resolution = %q, want 200 fhd", response.StatusCode, got.BroadcastResolution)
	}
	waitForPreviewSize(1920, 1080)
	// 되돌리면 다시 720p 핀이다.
	response = change(`{"resolution":"720p"}`)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("change back status = %d", response.StatusCode)
	}
	waitForPreviewSize(320, 180)
}

// 플랜이 허용하지 않는 해상도로는 바꿀 수 없다(#273 코드). 허용하는 플랜은 바뀐다.
func TestBroadcastResolutionChangeHonorsPlan(t *testing.T) {
	for _, test := range []struct {
		plan     plan.Plan
		wantCode string
		want     string
	}{
		{plan.Spark, "plan_resolution_not_allowed", session.Resolution720p},
		{plan.Beam, "", session.ResolutionFHD},
	} {
		t.Run(string(test.plan), func(t *testing.T) {
			server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{
				auth.StreamingProviderYouTube: &stubStreamingProvider{},
			})
			manager.SetPlanResolver(func(context.Context, uuid.UUID) (plan.Plan, error) { return test.plan, nil })
			live, ownerToken, err := manager.CreateForUserWithResolution(uuid.New(), session.DefaultProvider, "", session.Resolution720p, nil)
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequest(http.MethodPut, server.URL+"/sessions/"+live.ID+"/broadcast-resolution", strings.NewReader(`{"resolution":"fhd"}`))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-Session-Owner-Token", ownerToken)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{}
			_ = json.NewDecoder(response.Body).Decode(&payload)
			response.Body.Close()
			if test.wantCode != "" {
				if response.StatusCode != http.StatusForbidden || streamErrorCode(payload) != test.wantCode {
					t.Fatalf("status=%d code=%q, want 403 %s", response.StatusCode, streamErrorCode(payload), test.wantCode)
				}
			} else if response.StatusCode != http.StatusOK {
				t.Fatalf("status=%d payload=%v, want 200", response.StatusCode, payload)
			}
			if got := live.Resolution(); got != test.want {
				t.Fatalf("resolution = %q, want %q", got, test.want)
			}
		})
	}
}
