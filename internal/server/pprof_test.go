package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/origin"
	"inno-live-server/internal/session"
)

// pprof는 힙·고루틴 덤프와 프로세스 argv를 인증 없이 노출하므로 기본값이 꺼짐이어야
// 한다(#138). 라우트 등록 자체를 막는 구현이라 꺼진 상태에서는 401이 아니라 404가
// 나오는 것이 정상이다 — 존재 여부조차 알리지 않는다.
func TestPprofDisabledByDefault(t *testing.T) {
	application, manager := newPprofTestApplication(t, false)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/cmdline"} {
		response, err := http.Get(httpServer.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("PPROF_ENABLED 미설정인데 GET %s = %d, 404여야 한다", path, response.StatusCode)
		}
	}
}

// 프로파일링이 필요할 때는 켤 수 있어야 한다. cold-spike 입력 경로 판정처럼 실제
// 진단에 쓴 이력이 있어 도구 자체를 제거하지는 않는다.
func TestPprofEnabledByConfig(t *testing.T) {
	application, manager := newPprofTestApplication(t, true)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	response, err := http.Get(httpServer.URL + "/debug/pprof/heap")
	if err != nil {
		t.Fatalf("GET /debug/pprof/heap: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PPROF_ENABLED=true인데 GET /debug/pprof/heap = %d, 200이어야 한다", response.StatusCode)
	}
}

func newPprofTestApplication(t *testing.T, pprofEnabled bool) (*Server, *session.Manager) {
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
		PprofEnabled:            pprofEnabled,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := metrics.New()
	manager, err := session.NewManager(cfg, logger, registry, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	origins, err := origin.NewConfig(false, []string{"https://client.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, logger, registry, manager, nil, origins, nil, nil), manager
}
