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

// sessionExists reports whether the session is still readable by its owner.
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

	t.Run("stream start without token is 401", func(t *testing.T) {
		created, _ := createTestSession(t, httpServer.URL, nil)
		resp := mustRequest(t, http.MethodPost, httpServer.URL+"/sessions/"+created.SessionID+"/stream/start", nil, nil)
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

// TestSessionAuthDisabledBypass confirms the INNOLIVE_REQUIRE_SESSION_AUTH=false
// escape hatch: session-scoped routes work without a token (local dev only).
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
	httpServer := httptest.NewServer(New(cfg, logger, registry, manager, nil, origins, nil).Handler())
	defer httpServer.Close()

	created, _ := createTestSession(t, httpServer.URL, nil)
	resp := mustRequest(t, http.MethodDelete, httpServer.URL+"/sessions/"+created.SessionID, nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 when auth disabled", resp.StatusCode)
	}
}

// TestSessionHijackRejectedOverSignaling is the end-to-end reproduction of the
// issue over the real HTTP + WebSocket signaling path: victim A owns a session,
// attacker B knows the session_id but not the owner token. B's offer must be
// rejected at signaling (before the PeerConnection is touched), while A's offer
// with the correct token is answered — proving the hijack is closed without
// breaking the legitimate owner.
func TestSessionHijackRejectedOverSignaling(t *testing.T) {
	application, manager := newTestApplication(t)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	victim, victimToken := createTestSession(t, httpServer.URL, nil)

	// A real offer to feed the signaling channel.
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
		token any // omitted when nil
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

	// Owner with the correct token is answered.
	resp := send(map[string]any{"type": "offer", "session_id": victim.SessionID, "owner_token": victimToken, "sdp": offer.SDP})
	if resp["type"] != "answer" {
		t.Fatalf("owner offer response = %v, want answer", resp)
	}

	// The victim's session is intact after the hijack attempts.
	if !sessionExists(t, httpServer.URL, victim.SessionID, victimToken) {
		t.Fatal("victim session did not survive the hijack attempts")
	}
}

// errorCode pulls error.code out of a decoded signaling error response.
func errorCode(resp map[string]any) string {
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := errObj["code"].(string)
	return code
}
