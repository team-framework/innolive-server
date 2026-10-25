package server

import (
	"bytes"
	"encoding/json"
	"errors"
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

	"github.com/google/uuid"
)

// newHandoffTestServer는 제어 API를 루프백 httptest 서버로 띄운 Server다.
func newHandoffTestServer(t *testing.T) (*Server, *session.Manager, *httptest.Server) {
	t.Helper()
	manager, err := session.NewManager(config.Config{
		PrivacyMode:          config.PrivacyModeBypass,
		FFmpegPath:           "ffmpeg",
		UDPPortMin:           42300,
		UDPPortMax:           42400,
		FrameQueueSize:       2,
		RequireSessionAuth:   true,
		WebRTCRecoveryWindow: time.Minute,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	origins, err := origin.NewConfig(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	application := New(config.Config{AIControlToken: testAIControlToken}, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), manager, nil, origins, nil, nil)
	control := httptest.NewServer(application.AIControlHandler())
	t.Cleanup(control.Close)
	return application, manager, control
}

func postControl(t *testing.T, url, token string, body any) (int, map[string]any) {
	t.Helper()
	payload, _ := json.Marshal(body)
	request, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(response.Body).Decode(&decoded)
	return response.StatusCode, decoded
}

// 준비 단계 뒤에는 두 프로세스 모두 세션을 알고(Caddy를 돌려도 404가 없다), 확정
// 단계 뒤에는 새 프로세스만 같은 소유자 토큰으로 세션을 가진다(#517).
func TestSessionHandoffPrepareThenCommit(t *testing.T) {
	_, oldManager, oldControl := newHandoffTestServer(t)
	_, freshManager, freshControl := newHandoffTestServer(t)
	live, token, err := oldManager.CreateForUser(uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}

	code, body := postControl(t, oldControl.URL+"/sessions/handoff", testAIControlToken, map[string]string{"to": freshControl.URL, "phase": "prepare"})
	if code != http.StatusOK || body["handed"] != float64(1) || body["detached"] != float64(0) {
		t.Fatalf("prepare = %d %v", code, body)
	}
	if _, err := oldManager.Get(live.ID); err != nil {
		t.Fatalf("old process dropped the session in prepare: %v", err)
	}
	if _, err := freshManager.VerifyOwner(live.ID, token); err != nil {
		t.Fatalf("new process does not know the session after prepare: %v", err)
	}

	code, body = postControl(t, oldControl.URL+"/sessions/handoff", testAIControlToken, map[string]string{"to": freshControl.URL, "phase": "commit"})
	if code != http.StatusOK || body["detached"] != float64(1) {
		t.Fatalf("commit = %d %v", code, body)
	}
	if _, err := oldManager.Get(live.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("old process kept the session after commit: %v", err)
	}
	if _, err := freshManager.VerifyOwner(live.ID, token); err != nil {
		t.Fatalf("new process lost the session after commit: %v", err)
	}
}

// 넘기지 못하는 세션(게스트)은 옛 프로세스에 그대로 남는다.
func TestSessionHandoffSkipsGuestSessions(t *testing.T) {
	_, oldManager, oldControl := newHandoffTestServer(t)
	_, freshManager, freshControl := newHandoffTestServer(t)
	guest, _, err := oldManager.CreateForGuest("guest-a", nil)
	if err != nil {
		t.Fatal(err)
	}

	code, body := postControl(t, oldControl.URL+"/sessions/handoff", testAIControlToken, map[string]string{"to": freshControl.URL, "phase": "commit"})
	if code != http.StatusOK || body["handed"] != float64(0) {
		t.Fatalf("commit = %d %v", code, body)
	}
	if _, err := oldManager.Get(guest.ID); err != nil {
		t.Fatalf("guest session left the old process: %v", err)
	}
	if _, err := freshManager.Get(guest.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatal("guest session was imported")
	}
}

// 스트림 키가 오가므로 루프백이 아닌 대상과 토큰 없는 가져오기는 받지 않는다.
func TestSessionHandoffRejectsNonLoopbackTargetAndMissingToken(t *testing.T) {
	_, _, oldControl := newHandoffTestServer(t)
	for _, to := range []string{"http://10.0.0.5:9192", "https://127.0.0.1:9192", "http://example.com:9192", "http://127.0.0.1:9192/path"} {
		if code, body := postControl(t, oldControl.URL+"/sessions/handoff", testAIControlToken, map[string]string{"to": to, "phase": "prepare"}); code != http.StatusBadRequest {
			t.Fatalf("handoff to %q = %d %v, want 400", to, code, body)
		}
	}
	if code, _ := postControl(t, oldControl.URL+"/sessions/import", "wrong-token", map[string]string{}); code != http.StatusUnauthorized {
		t.Fatalf("import without token = %d, want 401", code)
	}
}
