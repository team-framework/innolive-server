package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"inno-live-server/internal/session"

	"github.com/google/uuid"
)

// 새 프로세스가 확정 import를 처리했는데 응답이 유실되면, 옛 프로세스는 실패로 보고
// 세션을 놓지 않는다. 그러면 세션이 양쪽에 남아 같은 스트림 키로 RTMP 연결이 둘
// 생긴다(P1-41). 확정 import는 다시 보내도 같은 결과이므로 다시 보내 확인한다.
func TestSessionHandoffCommitRetriesWhenImportResponseIsLost(t *testing.T) {
	_, oldManager, oldControl := newHandoffTestServer(t)
	_, freshManager, freshControl := newHandoffTestServer(t)
	live, token, err := oldManager.CreateForUser(uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	freshURL, _ := url.Parse(freshControl.URL)
	forward := httputil.NewSingleHostReverseProxy(freshURL)
	var dropped atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("commit") == "true" && dropped.CompareAndSwap(false, true) {
			// 새 프로세스가 처리하게 한 뒤 응답 없이 연결을 끊는다.
			forward.ServeHTTP(httptest.NewRecorder(), r)
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)

	code, body := postControl(t, oldControl.URL+"/sessions/handoff", testAIControlToken, map[string]string{"to": proxy.URL, "phase": "commit"})
	if code != http.StatusOK || body["detached"] != float64(1) {
		t.Fatalf("commit after a lost response = %d %v, want the session detached", code, body)
	}
	if _, err := oldManager.Get(live.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("old process kept the session: %v", err)
	}
	if _, err := freshManager.VerifyOwner(live.ID, token); err != nil {
		t.Fatalf("new process lost the session: %v", err)
	}
}

// 스냅샷에는 스트림 키가 들어 있다. 리다이렉트를 따라가면 다른 주소로 본문을 다시
// 보낸다(P1-41). 이관 대상은 루프백으로 검증한 주소 하나뿐이어야 한다.
func TestSessionHandoffDoesNotFollowRedirects(t *testing.T) {
	_, oldManager, oldControl := newHandoffTestServer(t)
	var leaked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		leaked.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(elsewhere.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	live, _, err := oldManager.CreateForUser(uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}

	code, body := postControl(t, oldControl.URL+"/sessions/handoff", testAIControlToken, map[string]string{"to": redirect.URL, "phase": "prepare"})
	if leaked.Load() != 0 {
		t.Fatalf("snapshot was re-sent to the redirect location %d times", leaked.Load())
	}
	failed, _ := body["failed"].(map[string]any)
	if code != http.StatusBadGateway || !strings.Contains(failed[live.ID].(string), "307") {
		t.Fatalf("prepare through a redirect = %d %v, want the handoff to fail", code, body)
	}
}

// 스냅샷을 뜬 뒤 옛 프로세스가 세션을 놓기까지(HTTP 최대 10초) 사이에 바뀐 설정은
// 새 프로세스에 가지 않는다(P1-39). 놓기 직전에 바뀌었으면 다시 떠서 보내야 한다.
func TestSessionHandoffCommitResendsChangesMadeDuringImport(t *testing.T) {
	_, oldManager, oldControl := newHandoffTestServer(t)
	_, freshManager, freshControl := newHandoffTestServer(t)
	live, _, err := oldManager.CreateForUser(uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	madeForKids := false
	freshURL, _ := url.Parse(freshControl.URL)
	forward := httputil.NewSingleHostReverseProxy(freshURL)
	var changed atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if changed.CompareAndSwap(false, true) {
			// import가 오가는 사이 사용자가 방송 제목을 바꾼다.
			if _, err := oldManager.SetBroadcastSettings(live.ID, session.YouTubeBroadcastSettings{Title: "바뀐 제목", MadeForKids: &madeForKids}); err != nil {
				t.Error(err)
			}
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)

	code, body := postControl(t, oldControl.URL+"/sessions/handoff", testAIControlToken, map[string]string{"to": proxy.URL, "phase": "commit"})
	if code != http.StatusOK || body["detached"] != float64(1) {
		t.Fatalf("commit = %d %v", code, body)
	}
	moved, err := freshManager.Get(live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.BroadcastSettings().Title != "바뀐 제목" {
		t.Fatal("new process did not receive the change made while the import was in flight")
	}
}
