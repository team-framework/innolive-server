package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

func restoreSignalingGuards(t *testing.T) {
	t.Helper()
	maxBytes := signalingMaxMessageBytes
	authTimeout := signalingAuthTimeout
	pongWait := signalingPongWait
	pingPeriod := signalingPingPeriod
	maxConns := signalingMaxConns
	maxPerIP := signalingMaxConnsPerIP
	t.Cleanup(func() {
		signalingMaxMessageBytes = maxBytes
		signalingAuthTimeout = authTimeout
		signalingPongWait = pongWait
		signalingPingPeriod = pingPeriod
		signalingMaxConns = maxConns
		signalingMaxConnsPerIP = maxPerIP
	})
}

func signalingWSURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http") + "/signaling"
}

func dialSignaling(t *testing.T, httpURL string, header http.Header) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(signalingWSURL(httpURL), header)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestSignalingRejectsOriginalOversizedReproBeforeParse(t *testing.T) {
	// 원 재현: 인증 없이 8,388,637바이트를 보낸다. 생산 한도(256KiB)를 낮추지 않는다.
	const originalReproBytes = 8388637
	if signalingMaxMessageBytes != 256<<10 {
		t.Fatalf("production read limit = %d, want %d", signalingMaxMessageBytes, 256<<10)
	}

	application, manager := newTestApplication(t)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	conn := dialSignaling(t, httpServer.URL, nil)
	payload := strings.Repeat("x", originalReproBytes)
	writeErr := conn.WriteMessage(websocket.TextMessage, []byte(payload))
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	readErr := conn.ReadJSON(&response)
	t.Logf("original repro payload=%d bytes production_limit=%d writeErr=%v readErr=%v", originalReproBytes, signalingMaxMessageBytes, writeErr, readErr)
	if readErr == nil {
		t.Fatalf("server parsed original oversized payload into %+v, want close before JSON", response)
	}
}

func TestSignalingRejectsOversizedMessageBeforeParse(t *testing.T) {
	restoreSignalingGuards(t)
	signalingMaxMessageBytes = 64

	application, manager := newTestApplication(t)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	conn := dialSignaling(t, httpServer.URL, nil)
	payload := strings.Repeat("x", int(signalingMaxMessageBytes)+1)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	err := conn.ReadJSON(&response)
	if err == nil {
		t.Fatalf("server parsed oversized payload into %+v, want close before JSON", response)
	}
}

func TestSignalingAuthTimeoutDefaultIsNegotiationWindow(t *testing.T) {
	if signalingAuthTimeout != 30*time.Second {
		t.Fatalf("production first-auth wait = %s, want 30s", signalingAuthTimeout)
	}
}

func TestSignalingClosesUnauthenticatedIdleConnection(t *testing.T) {
	restoreSignalingGuards(t)
	signalingAuthTimeout = 50 * time.Millisecond

	application, manager := newTestApplication(t)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	conn := dialSignaling(t, httpServer.URL, nil)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, _, err := conn.ReadMessage()
	elapsed := time.Since(started)
	t.Logf("unauthenticated idle close after %s (auth timeout=%s)", elapsed, signalingAuthTimeout)
	if err == nil {
		t.Fatal("expected server to close idle unauthenticated connection")
	}
	if elapsed > time.Second {
		t.Fatalf("idle close took %s, want around %s", elapsed, signalingAuthTimeout)
	}
}

func TestSignalingGlobalConnectionCap(t *testing.T) {
	restoreSignalingGuards(t)
	signalingMaxConns = 2
	signalingMaxConnsPerIP = 8

	application, manager := newTestApplication(t)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	first := dialSignaling(t, httpServer.URL, nil)
	second := dialSignaling(t, httpServer.URL, nil)
	if first == nil || second == nil {
		t.Fatal("expected first two upgrades to succeed")
	}

	conn, resp, err := websocket.DefaultDialer.Dial(signalingWSURL(httpServer.URL), nil)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("third signaling connection succeeded, want 503")
	}
	if resp == nil {
		t.Fatal("third signaling connection returned no HTTP response")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("third connection status = %d body=%s, want 503", resp.StatusCode, body)
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != "capacity_exceeded" {
		t.Fatalf("error code = %q, want capacity_exceeded", payload.Error.Code)
	}
}

func TestSignalingEmptyTrustedProxySkipsPerIPCap(t *testing.T) {
	restoreSignalingGuards(t)
	signalingMaxConns = 64
	signalingMaxConnsPerIP = 1

	application, manager := newTestApplication(t)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	first := dialSignaling(t, httpServer.URL, nil)
	second := dialSignaling(t, httpServer.URL, nil)
	if first == nil || second == nil {
		t.Fatal("empty trusted-proxy CIDRs should not apply the per-IP cap")
	}
}

func TestSignalingPerIPConnectionCap(t *testing.T) {
	restoreSignalingGuards(t)
	signalingMaxConns = 64
	signalingMaxConnsPerIP = 1

	cfg := testServerConfig()
	cfg.GuestQueueTrustedProxies = []string{"127.0.0.1/32", "::1/128"}
	application, manager := newTestApplicationWithConfig(t, cfg, nil)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	_ = dialSignaling(t, httpServer.URL, nil)
	conn, resp, err := websocket.DefaultDialer.Dial(signalingWSURL(httpServer.URL), nil)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("second connection from same IP succeeded, want 503")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("same-IP status = %v, want 503", resp)
	}
	if resp != nil {
		resp.Body.Close()
	}
}

func TestSignalingTrustedProxySplitsPerIP(t *testing.T) {
	restoreSignalingGuards(t)
	signalingMaxConns = 64
	signalingMaxConnsPerIP = 1

	cfg := testServerConfig()
	cfg.GuestQueueTrustedProxies = []string{"127.0.0.1/32", "::1/128"}
	application, manager := newTestApplicationWithConfig(t, cfg, nil)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	_ = dialSignaling(t, httpServer.URL, http.Header{"X-Forwarded-For": []string{"198.51.100.10"}})
	second := dialSignaling(t, httpServer.URL, http.Header{"X-Forwarded-For": []string{"198.51.100.11"}})
	if second == nil {
		t.Fatal("different forwarded clients should not share the per-IP cap")
	}
	conn, resp, err := websocket.DefaultDialer.Dial(signalingWSURL(httpServer.URL), http.Header{"X-Forwarded-For": []string{"198.51.100.10"}})
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("repeat forwarded IP succeeded, want 503")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("repeat forwarded IP status = %v, want 503", resp)
	}
	if resp != nil {
		resp.Body.Close()
	}
}

func TestSignalingAuthenticatedIdleClosesWithoutPong(t *testing.T) {
	restoreSignalingGuards(t)
	signalingPongWait = 200 * time.Millisecond
	signalingPingPeriod = 50 * time.Millisecond

	application, manager := newTestApplication(t)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	created, ownerToken := createTestSession(t, httpServer.URL, nil)
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

	conn := dialSignaling(t, httpServer.URL, nil)
	conn.SetPingHandler(func(string) error { return nil })
	if err := conn.WriteJSON(map[string]any{
		"type":        "offer",
		"session_id":  created.SessionID,
		"owner_token": ownerToken,
		"sdp":         offer.SDP,
	}); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var answer map[string]any
	if err := conn.ReadJSON(&answer); err != nil {
		t.Fatal(err)
	}
	if answer["type"] != "answer" {
		t.Fatalf("owner offer response = %v, want answer", answer)
	}

	started := time.Now()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.ReadMessage()
	elapsed := time.Since(started)
	t.Logf("authenticated idle close after %s (pong wait=%s)", elapsed, signalingPongWait)
	if err == nil {
		t.Fatal("expected authenticated idle connection to close without pong")
	}
	if elapsed > time.Second {
		t.Fatalf("authenticated idle close took %s, want around %s", elapsed, signalingPongWait)
	}
}
