package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/session"

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

	created := createTestSession(t, httpServer.URL, map[string]string{"source": "test"})
	if created.Status != "active" || created.Metadata["source"] != "test" {
		t.Fatalf("unexpected session response: %+v", created)
	}
	if created.Media.RawVideoTrack != nil || created.Media.ProcessedVideoTrack != nil {
		t.Fatalf("new session unexpectedly has media tracks: %+v", created.Media)
	}

	response := mustRequest(t, http.MethodGet, httpServer.URL+"/sessions", nil, nil)
	defer response.Body.Close()
	var listed struct {
		Sessions []session.Response `json:"sessions"`
	}
	mustDecode(t, response.Body, &listed)
	if len(listed.Sessions) != 1 || listed.Sessions[0].SessionID != created.SessionID {
		t.Fatalf("list response = %+v", listed)
	}

	response = mustRequest(t, http.MethodGet, httpServer.URL+"/metrics", nil, nil)
	metricsBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(metricsBody), "innolive_active_sessions 1") || !strings.Contains(string(metricsBody), "innolive_frame_processing_duration_seconds") {
		t.Fatalf("metrics response missing required series:\n%s", metricsBody)
	}

	response = mustRequest(t, http.MethodDelete, httpServer.URL+"/sessions/"+created.SessionID, nil, nil)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d", response.StatusCode)
	}
	response = mustRequest(t, http.MethodGet, httpServer.URL+"/sessions/"+created.SessionID, nil, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("GET deleted session status = %d", response.StatusCode)
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

func TestBypassWebRTCEndToEnd(t *testing.T) {
	application, manager := newTestApplication(t)
	defer manager.CloseAll()
	httpServer := httptest.NewServer(application.Handler())
	defer httpServer.Close()
	liveSession := createTestSession(t, httpServer.URL, nil)

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
		message := map[string]any{"type": "ice_candidate", "session_id": liveSession.SessionID, "candidate": nil}
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
	if err := writeSignal(map[string]any{"type": "offer", "session_id": liveSession.SessionID, "sdp": offer.SDP}); err != nil {
		t.Fatal(err)
	}

	answerDeadline := time.Now().Add(10 * time.Second)
	for {
		if err := connection.SetReadDeadline(answerDeadline); err != nil {
			t.Fatal(err)
		}
		var message struct {
			Type  string          `json:"type"`
			SDP   string          `json:"sdp"`
			Error json.RawMessage `json:"error"`
		}
		if err := connection.ReadJSON(&message); err != nil {
			t.Fatalf("read signaling response: %v", err)
		}
		if message.Type == "error" {
			t.Fatalf("signaling failed: %s", message.Error)
		}
		if message.Type != "answer" {
			continue
		}
		if err := peerConnection.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: message.SDP}); err != nil {
			t.Fatalf("set remote answer: %v", err)
		}
		break
	}

	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("PeerConnection did not reach connected state")
	}

	for _, input := range generateVP8Frames(t, 10) {
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
		t.Fatal("did not receive transcoded bypass video frame")
	}

	metricsResponse := mustRequest(t, http.MethodGet, httpServer.URL+"/metrics", nil, nil)
	metricsBody, _ := io.ReadAll(metricsResponse.Body)
	metricsResponse.Body.Close()
	for _, name := range []string{`innolive_frame_received_total{mode="bypass"}`, `innolive_frame_processed_total{mode="bypass"}`} {
		if value := prometheusValue(t, string(metricsBody), name); value < 1 {
			t.Errorf("metric %s = %v, want at least 1\n%s", name, value, metricsBody)
		}
	}

	deleteResponse := mustRequest(t, http.MethodDelete, httpServer.URL+"/sessions/"+liveSession.SessionID, nil, nil)
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
		"-frames:v", fmt.Sprintf("%d", count), "-c:v", "libvpx", "-deadline", "realtime", "-g", "30",
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
	t.Helper()
	cfg := config.Config{
		HTTPAddr:                ":0",
		PrivacyMode:             config.PrivacyModeBypass,
		PrivacyFixedDelay:       time.Millisecond,
		AITimeout:               time.Second,
		FFmpegPath:              "ffmpeg",
		UDPPortMin:              41000,
		UDPPortMax:              41100,
		DisconnectedGracePeriod: 100 * time.Millisecond,
		FrameQueueSize:          2,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := metrics.New()
	manager, err := session.NewManager(cfg, logger, registry, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, logger, registry, manager, nil), manager
}

func createTestSession(t *testing.T, baseURL string, metadata map[string]string) session.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"metadata": metadata})
	if err != nil {
		t.Fatal(err)
	}
	response := mustRequest(t, http.MethodPost, baseURL+"/sessions", bytes.NewReader(body), http.Header{"Content-Type": []string{"application/json"}})
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("POST /sessions status=%d body=%s", response.StatusCode, data)
	}
	var result session.Response
	mustDecode(t, response.Body, &result)
	return result
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
