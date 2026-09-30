package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/origin"
	"inno-live-server/internal/session"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

// sessionExists는 소유자가 세션을 아직 읽을 수 있는지다.
func sessionExists(t *testing.T, baseURL, id, token string) bool {
	t.Helper()
	resp := mustRequest(t, http.MethodGet, baseURL+"/sessions/"+id, nil, bearer(token))
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func TestSessionOwnershipEnforcement(t *testing.T) {
	application, manager := newTestApplication(t)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	t.Run("delete without token is 401 and session survives", func(t *testing.T) {
		created, token := createTestSession(t, httpServer.URL, nil)
		resp := mustRequest(t, http.MethodDelete, httpServer.URL+"/sessions/"+created.SessionID, nil, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if !sessionExists(t, httpServer.URL, created.SessionID, token) {
			t.Fatal("session was deleted despite missing token")
		}
	})

	t.Run("delete with another owner's token is 403 and session survives", func(t *testing.T) {
		victim, victimToken := createTestSession(t, httpServer.URL, nil)
		_, attackerToken := createTestSession(t, httpServer.URL, nil)
		resp := mustRequest(t, http.MethodDelete, httpServer.URL+"/sessions/"+victim.SessionID, nil, bearer(attackerToken))
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
		if !sessionExists(t, httpServer.URL, victim.SessionID, victimToken) {
			t.Fatal("victim session was deleted by a non-owner token")
		}
	})

	t.Run("unknown session id with a token is 404", func(t *testing.T) {
		resp := mustRequest(t, http.MethodDelete, httpServer.URL+"/sessions/does-not-exist", nil, bearer("whatever"))
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("delete with the correct token removes the session", func(t *testing.T) {
		created, token := createTestSession(t, httpServer.URL, nil)
		resp := mustRequest(t, http.MethodDelete, httpServer.URL+"/sessions/"+created.SessionID, nil, bearer(token))
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", resp.StatusCode)
		}
		if sessionExists(t, httpServer.URL, created.SessionID, token) {
			t.Fatal("session still exists after owner delete")
		}
	})

	t.Run("stream prepare without token is 401", func(t *testing.T) {
		created, _ := createTestSession(t, httpServer.URL, nil)
		resp := mustRequest(t, http.MethodPost, httpServer.URL+"/sessions/"+created.SessionID+"/stream/prepare", nil, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("reusing a deleted session's token is 404", func(t *testing.T) {
		created, token := createTestSession(t, httpServer.URL, nil)
		resp := mustRequest(t, http.MethodDelete, httpServer.URL+"/sessions/"+created.SessionID, nil, bearer(token))
		resp.Body.Close()
		resp = mustRequest(t, http.MethodDelete, httpServer.URL+"/sessions/"+created.SessionID, nil, bearer(token))
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
}

// TestSessionAuthDisabledBypass: INNOLIVE_REQUIRE_SESSION_AUTH=false 우회로를
// 확인한다. 세션 범위 경로가 토큰 없이 동작한다(로컬 개발 전용).
func TestSessionAuthDisabledBypass(t *testing.T) {
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
		RequireSessionAuth:      false,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := metrics.New()
	manager, err := session.NewManager(cfg, logger, registry, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.CloseAll()
	origins, err := origin.NewConfig(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(New(cfg, logger, registry, manager, nil, origins, nil, nil).Handler())
	defer httpServer.Close()

	created, _ := createTestSession(t, httpServer.URL, nil)
	resp := mustRequest(t, http.MethodDelete, httpServer.URL+"/sessions/"+created.SessionID, nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 when auth disabled", resp.StatusCode)
	}
}

// TestSessionHijackRejectedOverSignaling은 실제 HTTP + WebSocket signaling 경로로
// 이슈를 끝까지 재현한다. 피해자 A가 세션을 가지고, 공격자 B는 session_id는 알지만
// 소유자 토큰은 모른다. B의 offer는 signaling에서(PeerConnection을 건드리기 전에)
// 거절되고, 올바른 토큰을 가진 A의 offer에는 answer가 와야 한다 — 정당한 소유자를
// 깨지 않고 탈취를 막았음을 보인다.
func TestSessionHijackRejectedOverSignaling(t *testing.T) {
	application, manager := newTestApplication(t)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	victim, victimToken := createTestSession(t, httpServer.URL, nil)

	// signaling 채널에 넣을 실제 offer.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/signaling"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	send := func(msg map[string]any) map[string]any {
		t.Helper()
		if err := conn.WriteJSON(msg); err != nil {
			t.Fatal(err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var resp map[string]any
		if err := conn.ReadJSON(&resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	cases := []struct {
		name  string
		token any // nil이면 생략
	}{
		{name: "stolen token", token: "stolen-or-guessed"},
		{name: "no token", token: nil},
	}
	for _, tc := range cases {
		msg := map[string]any{"type": "offer", "session_id": victim.SessionID, "sdp": offer.SDP}
		if tc.token != nil {
			msg["owner_token"] = tc.token
		}
		resp := send(msg)
		if resp["type"] != "error" || errorCode(resp) != "forbidden" {
			t.Fatalf("attacker offer (%s) response = %v, want error/forbidden", tc.name, resp)
		}
	}

	// 올바른 토큰을 가진 소유자에게는 answer가 온다.
	resp := send(map[string]any{"type": "offer", "session_id": victim.SessionID, "owner_token": victimToken, "sdp": offer.SDP})
	if resp["type"] != "answer" {
		t.Fatalf("owner offer response = %v, want answer", resp)
	}

	// 탈취 시도 뒤에도 피해자의 세션은 그대로다.
	if !sessionExists(t, httpServer.URL, victim.SessionID, victimToken) {
		t.Fatal("victim session did not survive the hijack attempts")
	}
}

// errorCode는 디코드한 signaling 오류 응답에서 error.code를 꺼낸다.
func errorCode(resp map[string]any) string {
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := errObj["code"].(string)
	return code
}
