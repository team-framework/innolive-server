package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/origin"
	"inno-live-server/internal/session"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

func TestSessionLifecycleAndMetricsAPI(t *testing.T) {
	application, manager := newTestApplication(t)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	created, ownerToken := createTestSession(t, httpServer.URL, map[string]string{"source": "test"})
	if created.Status != "active" || created.Metadata["source"] != "test" {
		t.Fatalf("unexpected session response: %+v", created)
	}
	if created.Media.RawVideoTrack != nil || created.Media.ProcessedVideoTrack != nil {
		t.Fatalf("new session unexpectedly has media tracks: %+v", created.Media)
	}

	// The owner can read their own session with the token.
	response := mustRequest(t, http.MethodGet, httpServer.URL+"/sessions/"+created.SessionID, nil, bearer(ownerToken))
	var fetched session.Response
	mustDecode(t, response.Body, &fetched)
	response.Body.Close()
	if fetched.SessionID != created.SessionID {
		t.Fatalf("GET own session = %+v", fetched)
	}

	response = mustRequest(t, http.MethodGet, httpServer.URL+"/metrics", nil, nil)
	metricsBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(metricsBody), "innolive_active_sessions 1") || !strings.Contains(string(metricsBody), "innolive_frame_processing_duration_seconds") {
		t.Fatalf("metrics response missing required series:\n%s", metricsBody)
	}

	response = mustRequest(t, http.MethodDelete, httpServer.URL+"/sessions/"+created.SessionID, nil, bearer(ownerToken))
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d", response.StatusCode)
	}
	response = mustRequest(t, http.MethodGet, httpServer.URL+"/sessions/"+created.SessionID, nil, bearer(ownerToken))
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("GET deleted session status = %d", response.StatusCode)
	}
	var missingSession struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	mustDecode(t, response.Body, &missingSession)
	if missingSession.Error.Code != "not_found" {
		t.Fatalf("GET deleted session error code = %q, want not_found", missingSession.Error.Code)
	}
}

func TestCORSAndSignalingValidation(t *testing.T) {
	application, manager := newTestApplication(t)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	headers := http.Header{
		"Origin":                         []string{"https://client.invalid"},
		"Access-Control-Request-Method":  []string{"POST"},
		"Access-Control-Request-Headers": []string{"content-type,x-custom-header"},
	}
	response := mustRequest(t, http.MethodOptions, httpServer.URL+"/sessions", nil, headers)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Access-Control-Allow-Origin") != "https://client.invalid" {
		t.Fatalf("unexpected CORS response: status=%d headers=%v", response.StatusCode, response.Header)
	}

	disallowedResponse := mustRequest(t, http.MethodGet, httpServer.URL+"/webrtc/config", nil, http.Header{"Origin": []string{"https://evil.example"}})
	disallowedResponse.Body.Close()
	if disallowedResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("disallowed HTTP origin status = %d, want %d", disallowedResponse.StatusCode, http.StatusForbidden)
	}

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/signaling"
	connection, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.WriteMessage(websocket.TextMessage, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	var signalError struct {
		Type  string `json:"type"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := connection.ReadJSON(&signalError); err != nil {
		t.Fatal(err)
	}
	if signalError.Type != "error" || signalError.Error.Code != "bad_request" {
		t.Fatalf("unexpected signaling error: %+v", signalError)
	}
}

type testUserChecker struct {
	status auth.UserStatus
	seen   []uuid.UUID
}

func (c *testUserChecker) UserStatus(_ context.Context, userID uuid.UUID) (auth.UserStatus, error) {
	c.seen = append(c.seen, userID)
	return c.status, nil
}

func testRequireUser(t *testing.T) (func(http.Handler) http.Handler, func(context.Context, string) (uuid.UUID, error), http.Header, *testUserChecker, uuid.UUID) {
	t.Helper()
	key := []byte("0123456789abcdef0123456789abcdef")
	userID := uuid.New()
	now := time.Now().UTC()
	claims := auth.AccessClaims{
		SessionID: uuid.NewString(),
		TokenType: "access",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "test-server",
			Subject:   userID.String(),
			Audience:  jwt.ClaimStrings{"test-api"},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
			NotBefore: jwt.NewNumericDate(now.Add(-time.Second)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        uuid.NewString(),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	service := auth.NewTokenService(nil, auth.TokenConfig{
		AccessKey: key, AccessTTL: time.Minute, RefreshTTL: time.Hour, RefreshAbsoluteTTL: time.Hour,
		Issuer: "test-server", Audience: "test-api",
	})
	checker := &testUserChecker{status: auth.UserStatusActive}
	return auth.RequireUser(service, checker), func(ctx context.Context, raw string) (uuid.UUID, error) {
		return auth.AuthenticateUser(ctx, service, checker, raw)
	}, bearer(token), checker, userID
}

func TestUserScopedRoutesRequireValidatedLogin(t *testing.T) {
	requireUser, authenticateUser, accessHeader, checker, userID := testRequireUser(t)
	application, manager := newTestApplicationWithUserMiddleware(t, requireUser, authenticateUser)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	protectedRoutes := []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodPost, path: "/sessions", want: http.StatusCreated},
		{method: http.MethodGet, path: "/webrtc/config", want: http.StatusOK},
		{method: http.MethodGet, path: "/reference-face", want: http.StatusOK},
		{method: http.MethodPost, path: "/reference-face", want: http.StatusBadRequest},
		{method: http.MethodDelete, path: "/reference-face", want: http.StatusBadRequest},
		{method: http.MethodDelete, path: "/reference-face/not-found", want: http.StatusBadRequest},
	}
	for _, protected := range protectedRoutes {
		response := mustRequest(t, protected.method, httpServer.URL+protected.path, nil, nil)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s %s status = %d, want %d", protected.method, protected.path, response.StatusCode, http.StatusUnauthorized)
		}

		response = mustRequest(t, protected.method, httpServer.URL+protected.path, nil, accessHeader)
		response.Body.Close()
		if response.StatusCode != protected.want {
			t.Fatalf("authenticated %s %s status = %d, want %d", protected.method, protected.path, response.StatusCode, protected.want)
		}
	}
	// Session-scoped routes require both an access token and the separate
	// capability token. Keeping the latter out of Authorization prevents one
	// credential from replacing the other.
	createResponse := mustRequest(t, http.MethodPost, httpServer.URL+"/sessions", nil, accessHeader)
	var created struct {
		session.Response
		OwnerToken string `json:"owner_token"`
	}
	mustDecode(t, createResponse.Body, &created)
	createResponse.Body.Close()
	liveSession, err := manager.Get(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if liveSession.UserID != userID {
		t.Fatalf("session user ID = %s, want %s", liveSession.UserID, userID)
	}
	if liveSession.AIClientID != session.AIClientIDForUser(userID) {
		t.Fatalf("session AI client ID = %q, want %q", liveSession.AIClientID, session.AIClientIDForUser(userID))
	}
	ownerHeaders := accessHeader.Clone()
	ownerHeaders.Set("X-Session-Owner-Token", created.OwnerToken)
	ownerResponse := mustRequest(t, http.MethodGet, httpServer.URL+"/sessions/"+created.SessionID, nil, ownerHeaders)
	ownerResponse.Body.Close()
	if ownerResponse.StatusCode != http.StatusOK {
		t.Fatalf("session owner status = %d, want %d", ownerResponse.StatusCode, http.StatusOK)
	}
	_, _, attackerHeader, _, attackerID := testRequireUser(t)
	attackerHeaders := attackerHeader.Clone()
	attackerHeaders.Set("X-Session-Owner-Token", created.OwnerToken)
	attackerResponse := mustRequest(t, http.MethodGet, httpServer.URL+"/sessions/"+created.SessionID, nil, attackerHeaders)
	attackerResponse.Body.Close()
	if attackerResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("different authenticated user status = %d, want %d", attackerResponse.StatusCode, http.StatusForbidden)
	}
	if attackerID == userID {
		t.Fatal("test users unexpectedly have the same ID")
	}
	accessOnlyResponse := mustRequest(t, http.MethodGet, httpServer.URL+"/sessions/"+created.SessionID, nil, accessHeader)
	accessOnlyResponse.Body.Close()
	if accessOnlyResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("access token must not replace session owner token: status = %d, want %d", accessOnlyResponse.StatusCode, http.StatusUnauthorized)
	}

	if len(checker.seen) != len(protectedRoutes)+4 {
		t.Fatalf("active-user checks = %d, want %d", len(checker.seen), len(protectedRoutes)+4)
	}

	referenceResponse := mustRequest(t, http.MethodGet, httpServer.URL+"/reference-face?client_id=another-user", nil, accessHeader)
	var reference referenceStatus
	mustDecode(t, referenceResponse.Body, &reference)
	referenceResponse.Body.Close()
	if reference.ClientID != session.AIClientIDForUser(userID) {
		t.Fatalf("reference client ID = %q, want server-derived %q", reference.ClientID, session.AIClientIDForUser(userID))
	}
	for _, seenUserID := range checker.seen {
		if seenUserID != userID && seenUserID != attackerID {
			t.Fatalf("checked unexpected user ID = %s", seenUserID)
		}
	}

	for _, path := range []string{"/", "/health", "/metrics"} {
		response := mustRequest(t, http.MethodGet, httpServer.URL+path, nil, nil)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("public GET %s status = %d, want %d", path, response.StatusCode, http.StatusOK)
		}
	}
}

func TestSignalingRequiresSessionOwnerUser(t *testing.T) {
	requireUser, authenticateUser, accessHeader, _, _ := testRequireUser(t)
	application, manager := newTestApplicationWithUserMiddleware(t, requireUser, authenticateUser)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	created, ownerToken := createTestSessionWithHeaders(t, httpServer.URL, nil, accessHeader)
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
	connection, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	send := func(values map[string]any) map[string]any {
		t.Helper()
		if err := connection.WriteJSON(values); err != nil {
			t.Fatal(err)
		}
		if err := connection.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var response map[string]any
		if err := connection.ReadJSON(&response); err != nil {
			t.Fatal(err)
		}
		return response
	}

	base := map[string]any{"type": "offer", "session_id": created.SessionID, "owner_token": ownerToken, "sdp": offer.SDP}
	if response := send(base); response["type"] != "error" || errorCode(response) != "unauthorized" {
		t.Fatalf("missing access token response = %v, want error/unauthorized", response)
	}
	_, _, attackerHeader, _, _ := testRequireUser(t)
	attackerToken, _ := bearerToken(&http.Request{Header: attackerHeader})
	attacker := maps.Clone(base)
	attacker["access_token"] = attackerToken
	if response := send(attacker); response["type"] != "error" || errorCode(response) != "forbidden" {
		t.Fatalf("different user response = %v, want error/forbidden", response)
	}
	accessToken, _ := bearerToken(&http.Request{Header: accessHeader})
	owner := maps.Clone(base)
	owner["access_token"] = accessToken
	if response := send(owner); response["type"] != "answer" {
		t.Fatalf("session owner response = %v, want answer", response)
	}
}

func TestSignalingOriginPolicyRejectsDisallowedWebSocketOrigin(t *testing.T) {
	application, manager := newTestApplication(t)
	defer manager.CloseAll()

	// Mount the signaling handler directly so this test exercises the
	// websocket.Upgrader CheckOrigin callback, independently of HTTP CORS.
	httpServer := httptest.NewServer(http.HandlerFunc(application.handleSignaling))
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/signaling"

	_, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{"https://evil.example"}})
	if err == nil {
		t.Fatal("disallowed websocket origin unexpectedly connected")
	}
	if response == nil || response.StatusCode != http.StatusForbidden {
		if response == nil {
			t.Fatal("disallowed websocket origin returned no HTTP response")
		}
		t.Fatalf("disallowed websocket status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}

	connection, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{"https://client.invalid"}})
	if err != nil {
		if response != nil {
			t.Fatalf("allowed websocket origin status = %d: %v", response.StatusCode, err)
		}
		t.Fatal(err)
	}
	connection.Close()
}

func TestWebRTCConfigPublishesRecoveryContract(t *testing.T) {
	application, manager := newTestApplication(t)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	response := mustRequest(t, http.MethodGet, httpServer.URL+"/webrtc/config", nil, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /webrtc/config status = %d", response.StatusCode)
	}
	var payload struct {
		Recovery struct {
			WindowMS    int64 `json:"window_ms"`
			DebounceMS  int64 `json:"debounce_ms"`
			MaxAttempts int   `json:"max_attempts"`
		} `json:"recovery"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Recovery.WindowMS != 50000 || payload.Recovery.DebounceMS != 2000 || payload.Recovery.MaxAttempts != 10 {
		t.Fatalf("recovery = %+v, want 50000ms/2000ms/10", payload.Recovery)
	}
}

func TestSignalingTricklesAnswerBeforeSTUNGatheringCompletes(t *testing.T) {
	cfg := testServerConfig()
	// TEST-NET 주소는 STUN 응답을 주지 않는다. 이전처럼 gathering complete까지
	// 기다리면 이 테스트의 짧은 deadline 안에 answer를 보낼 수 없다.
	cfg.STUNURLs = []string{"stun:192.0.2.1:3478"}
	application, manager := newTestApplicationWithConfig(t, cfg, nil)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	liveSession, ownerToken := createTestSession(t, httpServer.URL, nil)

	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer peerConnection.Close()
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		"video",
		"trickle-answer-test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peerConnection.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}); err != nil {
		t.Fatal(err)
	}
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := peerConnection.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/signaling"
	connection, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	negotiationID := uuid.NewString()
	startedAt := time.Now()
	if err := connection.WriteJSON(map[string]any{
		"type":           "offer",
		"session_id":     liveSession.SessionID,
		"owner_token":    ownerToken,
		"sdp":            offer.SDP,
		"negotiation_id": negotiationID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(startedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Type          string `json:"type"`
		NegotiationID string `json:"negotiation_id"`
	}
	if err := connection.ReadJSON(&response); err != nil {
		t.Fatalf("read trickled answer: %v", err)
	}
	if response.Type != "answer" || response.NegotiationID != negotiationID {
		t.Fatalf("first signaling response = %+v, want answer for %s", response, negotiationID)
	}
}

func TestBypassWebRTCEndToEnd(t *testing.T) {
	application, manager := newTestApplication(t)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	liveSession, ownerToken := createTestSession(t, httpServer.URL, nil)

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/signaling"
	connection, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	var writeMu sync.Mutex
	writeSignal := func(value any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return connection.WriteJSON(value)
	}

	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer peerConnection.Close()
	negotiationID := uuid.NewString()
	connected := make(chan struct{}, 1)
	received := make(chan []byte, 1)
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})
	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		message := map[string]any{"type": "ice_candidate", "session_id": liveSession.SessionID, "owner_token": ownerToken, "negotiation_id": negotiationID, "candidate": nil}
		if candidate != nil {
			jsonCandidate := candidate.ToJSON()
			message["candidate"] = jsonCandidate.Candidate
			message["sdpMid"] = jsonCandidate.SDPMid
			message["sdpMLineIndex"] = jsonCandidate.SDPMLineIndex
		}
		_ = writeSignal(message)
	})
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			var assembled []byte
			for {
				packet, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				var vp8 codecs.VP8Packet
				payload, err := vp8.Unmarshal(packet.Payload)
				if err != nil {
					return
				}
				assembled = append(assembled, payload...)
				if packet.Marker {
					received <- assembled
					return
				}
			}
		}()
	})

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		"video",
		"integration-test",
	)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := peerConnection.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			if _, _, err := sender.Sender().ReadRTCP(); err != nil {
				return
			}
		}
	}()

	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := peerConnection.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	if err := writeSignal(map[string]any{"type": "offer", "session_id": liveSession.SessionID, "owner_token": ownerToken, "sdp": offer.SDP, "negotiation_id": negotiationID, "ice_restart": false}); err != nil {
		t.Fatal(err)
	}

	answerDeadline := time.Now().Add(10 * time.Second)
	answerApplied := false
	serverCandidateCount := 0
	serverCandidateGatheringComplete := false
	var pendingServerCandidates []webrtc.ICECandidateInit
	for !(answerApplied && serverCandidateGatheringComplete) {
		if err := connection.SetReadDeadline(answerDeadline); err != nil {
			t.Fatal(err)
		}
		var message struct {
			Type          string          `json:"type"`
			SDP           string          `json:"sdp"`
			NegotiationID string          `json:"negotiation_id"`
			Candidate     *string         `json:"candidate"`
			SDPMid        *string         `json:"sdpMid"`
			SDPLineIndex  *uint16         `json:"sdpMLineIndex"`
			Error         json.RawMessage `json:"error"`
		}
		if err := connection.ReadJSON(&message); err != nil {
			t.Fatalf("read signaling response: %v", err)
		}
		if message.Type == "error" {
			t.Fatalf("signaling failed: %s", message.Error)
		}
		switch message.Type {
		case "ice_candidate":
			if message.NegotiationID != negotiationID {
				t.Fatalf("server candidate negotiation_id = %q, want %q", message.NegotiationID, negotiationID)
			}
			candidate := webrtc.ICECandidateInit{
				SDPMid:        message.SDPMid,
				SDPMLineIndex: message.SDPLineIndex,
			}
			if message.Candidate == nil {
				serverCandidateGatheringComplete = true
			} else {
				candidate.Candidate = *message.Candidate
				serverCandidateCount++
			}
			if answerApplied {
				if err := peerConnection.AddICECandidate(candidate); err != nil {
					t.Fatalf("add trickled server ICE candidate: %v", err)
				}
			} else {
				pendingServerCandidates = append(pendingServerCandidates, candidate)
			}
			continue
		case "answer":
			if message.NegotiationID != negotiationID {
				t.Fatalf("answer negotiation_id = %q, want %q", message.NegotiationID, negotiationID)
			}
			if err := peerConnection.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: message.SDP}); err != nil {
				t.Fatalf("set remote answer: %v", err)
			}
			answerApplied = true
			for _, candidate := range pendingServerCandidates {
				if err := peerConnection.AddICECandidate(candidate); err != nil {
					t.Fatalf("add queued trickled server ICE candidate: %v", err)
				}
			}
			pendingServerCandidates = nil
		}
	}
	if serverCandidateCount == 0 {
		t.Fatal("server did not trickle any ICE candidates")
	}

	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("PeerConnection did not reach connected state")
	}

	// SampleBuilder retains the newest incomplete sample until it sees a later
	// RTP timestamp. Send a short stream rather than a single GOP so the final
	// frames are also released to the decoder while the peer remains connected.
	for _, input := range generateVP8Frames(t, 30) {
		if err := track.WriteSample(media.Sample{Data: input, Duration: time.Second / 30}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case output := <-received:
		if len(output) < 10 || output[0]&1 != 0 || !bytes.Equal(output[3:6], []byte{0x9d, 0x01, 0x2a}) {
			t.Fatalf("received frame is not a complete VP8 keyframe: %x", output)
		}
	case <-time.After(10 * time.Second):
		metricsResponse := mustRequest(t, http.MethodGet, httpServer.URL+"/metrics", nil, nil)
		metricsBody, _ := io.ReadAll(metricsResponse.Body)
		metricsResponse.Body.Close()
		t.Fatalf("did not receive transcoded bypass video frame:\n%s", metricsBody)
	}

	metricsResponse := mustRequest(t, http.MethodGet, httpServer.URL+"/metrics", nil, nil)
	metricsBody, _ := io.ReadAll(metricsResponse.Body)
	metricsResponse.Body.Close()
	for _, name := range []string{`innolive_frame_received_total{mode="bypass"}`, `innolive_frame_processed_total{mode="bypass"}`} {
		if value := prometheusValue(t, string(metricsBody), name); value < 1 {
			t.Errorf("metric %s = %v, want at least 1\n%s", name, value, metricsBody)
		}
	}

	deleteResponse := mustRequest(t, http.MethodDelete, httpServer.URL+"/sessions/"+liveSession.SessionID, nil, bearer(ownerToken))
	deleteResponse.Body.Close()
	if deleteResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE session status = %d", deleteResponse.StatusCode)
	}

	cleanupDeadline := time.Now().Add(5 * time.Second)
	for {
		cleanupResponse := mustRequest(t, http.MethodGet, httpServer.URL+"/metrics", nil, nil)
		cleanupBody, _ := io.ReadAll(cleanupResponse.Body)
		cleanupResponse.Body.Close()
		cleanupMetrics := string(cleanupBody)
		if prometheusValue(t, cleanupMetrics, "innolive_active_sessions") == 0 &&
			prometheusValue(t, cleanupMetrics, "innolive_processing_queue_size") == 0 {
			break
		}
		if time.Now().After(cleanupDeadline) {
			t.Fatalf("session cleanup did not reach active_sessions=0 and queue_size=0:\n%s", cleanupMetrics)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func generateVP8Frames(t *testing.T, count int) [][]byte {
	t.Helper()
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	command := exec.Command(
		ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x180:rate=30",
		// The sender emits this finite burst immediately after ICE connects. Make
		// every fixture frame a keyframe so a receiver that begins reading after
		// the first RTP packet can still initialize its VP8 decoder.
		"-frames:v", fmt.Sprintf("%d", count), "-c:v", "libvpx", "-deadline", "realtime", "-g", "1",
		"-f", "ivf", "pipe:1",
	)
	stream, err := command.Output()
	if err != nil {
		t.Fatalf("generate VP8 keyframe: %v", err)
	}
	if len(stream) < 32 || string(stream[:4]) != "DKIF" {
		t.Fatalf("FFmpeg returned an invalid IVF stream: %d bytes", len(stream))
	}
	frames := make([][]byte, 0, count)
	offset := 32
	for len(frames) < count {
		if offset+12 > len(stream) {
			t.Fatalf("FFmpeg returned %d of %d requested VP8 frames", len(frames), count)
		}
		size := int(binary.LittleEndian.Uint32(stream[offset : offset+4]))
		offset += 12
		if size <= 0 || offset+size > len(stream) {
			t.Fatalf("FFmpeg returned an invalid IVF frame size: %d", size)
		}
		frames = append(frames, append([]byte(nil), stream[offset:offset+size]...))
		offset += size
	}
	return frames
}

func prometheusValue(t *testing.T, text, name string) float64 {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, name+" ") {
			continue
		}
		var value float64
		if _, err := fmt.Sscanf(strings.TrimPrefix(line, name+" "), "%f", &value); err != nil {
			t.Fatalf("parse Prometheus metric %s: %v", name, err)
		}
		return value
	}
	t.Fatalf("Prometheus metric %s not found:\n%s", name, text)
	return 0
}

func newTestApplication(t *testing.T) (*Server, *session.Manager) {
	return newTestApplicationWithUserMiddleware(t, nil)
}

func newTestApplicationWithUserMiddleware(t *testing.T, requireUser func(http.Handler) http.Handler, authenticateUsers ...func(context.Context, string) (uuid.UUID, error)) (*Server, *session.Manager) {
	t.Helper()
	return newTestApplicationWithConfig(t, testServerConfig(), requireUser, authenticateUsers...)
}

func testServerConfig() config.Config {
	return config.Config{
		HTTPAddr:                ":0",
		PrivacyMode:             config.PrivacyModeBypass,
		PrivacyFixedDelay:       time.Millisecond,
		AITimeout:               time.Second,
		FFmpegPath:              "ffmpeg",
		UDPPortMin:              41000,
		UDPPortMax:              41100,
		DisconnectedGracePeriod: 100 * time.Millisecond,
		WebRTCRecoveryWindow:    50 * time.Second,
		WebRTCRecoveryDebounce:  2 * time.Second,
		WebRTCRecoveryAttempts:  10,
		FrameQueueSize:          2,
		RequireSessionAuth:      true,
	}
}

func newTestApplicationWithConfig(t *testing.T, cfg config.Config, requireUser func(http.Handler) http.Handler, authenticateUsers ...func(context.Context, string) (uuid.UUID, error)) (*Server, *session.Manager) {
	t.Helper()
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
	return New(cfg, logger, registry, manager, nil, origins, requireUser, nil, authenticateUsers...), manager
}

// createTestSession creates a session and returns its response plus the
// one-time owner token that later session-scoped calls must present.
func createTestSession(t *testing.T, baseURL string, metadata map[string]string) (session.Response, string) {
	return createTestSessionWithHeaders(t, baseURL, metadata, http.Header{"Content-Type": []string{"application/json"}})
}

func createTestSessionWithHeaders(t *testing.T, baseURL string, metadata map[string]string, headers http.Header) (session.Response, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"metadata": metadata})
	if err != nil {
		t.Fatal(err)
	}
	headers = headers.Clone()
	headers.Set("Content-Type", "application/json")
	response := mustRequest(t, http.MethodPost, baseURL+"/sessions", bytes.NewReader(body), headers)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("POST /sessions status=%d body=%s", response.StatusCode, data)
	}
	var result struct {
		session.Response
		OwnerToken string `json:"owner_token"`
	}
	mustDecode(t, response.Body, &result)
	if result.OwnerToken == "" {
		t.Fatal("POST /sessions did not return an owner_token")
	}
	return result.Response, result.OwnerToken
}

// bearer builds an Authorization header carrying the owner token.
func bearer(token string) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + token}}
}

func mustRequest(t *testing.T, method, url string, body io.Reader, headers http.Header) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func mustDecode(t *testing.T, reader io.Reader, target any) {
	t.Helper()
	if err := json.NewDecoder(reader).Decode(target); err != nil {
		t.Fatal(fmt.Errorf("decode response: %w", err))
	}
}

// 거절된 Origin이 로그에 남지 않으면 프로덕션에서 CORS 실패 원인을 좁힐 수
// 없다 — 브라우저는 요청 헤더를 개발자에게만 보여주고 서버에는 흔적이 없다.
func TestCORSRejectionLogsOrigin(t *testing.T) {
	origins, err := origin.NewConfig(false, []string{"https://client.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	handler := corsMiddleware(logger, origins, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	request := httptest.NewRequest(http.MethodGet, "/webrtc/config", nil)
	request.Header.Set("Origin", "https://evil.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	if got := logs.String(); !strings.Contains(got, "origin rejected") || !strings.Contains(got, "https://evil.example") {
		t.Fatalf("rejected origin was not logged: %q", got)
	}

	// Origin은 인증 전에 거절되는 클라이언트 제어 헤더라, 자르지 않으면
	// 누구나 반복 호출로 로그를 부풀릴 수 있다.
	logs.Reset()
	long := httptest.NewRequest(http.MethodGet, "/webrtc/config", nil)
	long.Header.Set("Origin", "https://"+strings.Repeat("a", 4096)+".example")
	handler.ServeHTTP(httptest.NewRecorder(), long)
	if got := logs.String(); strings.Contains(got, strings.Repeat("a", 513)) {
		t.Fatalf("oversized origin was not truncated: %d bytes logged", len(got))
	}
}

// CORS 거절 응답에 request_id가 없으면 사용자가 보여준 에러와 서버 로그를
// 묶을 방법이 없다. 미들웨어 순서가 뒤집히면 조용히 사라지는 성질이라 고정한다.
func TestCORSRejectionCarriesRequestIDInResponseAndLog(t *testing.T) {
	origins, err := origin.NewConfig(false, []string{"https://client.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	handler := requestIDMiddleware(corsMiddleware(logger, origins, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	request := httptest.NewRequest(http.MethodPost, "/sessions", nil)
	request.Header.Set("Origin", "https://evil.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	headerID := response.Header().Get("X-Request-ID")
	if headerID == "" {
		t.Fatal("X-Request-ID header is missing from the rejection response")
	}
	var payload struct {
		RequestID string `json:"request_id"`
	}
	mustDecode(t, response.Body, &payload)
	if payload.RequestID != headerID {
		t.Fatalf("body request_id = %q, header = %q", payload.RequestID, headerID)
	}
	if got := logs.String(); !strings.Contains(got, headerID) {
		t.Fatalf("log does not carry request_id %q: %q", headerID, got)
	}
}

// New()가 조립한 실제 핸들러에서도 순서가 유지되는지 확인한다.
func TestApplicationCORSRejectionCarriesRequestID(t *testing.T) {
	application, manager := newTestApplication(t)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()

	response := mustRequest(t, http.MethodPost, httpServer.URL+"/sessions", nil, http.Header{"Origin": []string{"https://evil.example"}})
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	if response.Header.Get("X-Request-ID") == "" {
		t.Fatal("X-Request-ID header is missing from the rejection response")
	}
	var payload struct {
		RequestID string `json:"request_id"`
	}
	mustDecode(t, response.Body, &payload)
	if payload.RequestID != response.Header.Get("X-Request-ID") {
		t.Fatalf("body request_id = %q, header = %q", payload.RequestID, response.Header.Get("X-Request-ID"))
	}
}
