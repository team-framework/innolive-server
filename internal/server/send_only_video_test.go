package server

import (
	"bytes"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"inno-live-server/internal/session"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturedVideoLog struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (w *capturedVideoLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.Write(p)
}
func (w *capturedVideoLog) text() string { w.mu.Lock(); defer w.mu.Unlock(); return w.data.String() }
func TestSendOnlyPublisherStillProcessesVideoAndCleansSession(t *testing.T) {
	logs := &capturedVideoLog{}
	app, manager := newTestApplicationWithLogger(t, testServerConfig(), nil, slog.New(slog.NewTextHandler(logs, nil)))
	defer manager.CloseAll()
	httpServer := httptest.NewServer(app.Handler())
	defer httpServer.Close()
	created, owner := createTestSession(t, httpServer.URL, nil)
	track, returned := connectTestPublisherDirection(t, httpServer.URL, created.SessionID, owner, webrtc.RTPTransceiverDirectionSendonly)
	for _, data := range generateVP8Frames(t, 30) {
		if err := track.WriteSample(media.Sample{Data: data, Duration: time.Second / 30}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		response := mustRequest(t, http.MethodGet, httpServer.URL+"/metrics", nil, nil)
		b, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if strings.Contains(string(b), `innolive_frame_processed_total{mode="bypass"} `) && prometheusValue(t, string(b), `innolive_frame_processed_total{mode="bypass"}`) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("send-only ingress was not processed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	encodingMetrics := mustRequest(t, http.MethodGet, httpServer.URL+"/metrics", nil, nil)
	encodingBody, _ := io.ReadAll(encodingMetrics.Body)
	encodingMetrics.Body.Close()
	if strings.Contains(string(encodingBody), "\ninnolive_encode_duration_seconds_count ") {
		if count := prometheusValue(t, string(encodingBody), "innolive_encode_duration_seconds_count"); count != 0 {
			t.Fatalf("send-only unnecessarily encoded %v return frames", count)
		}
	}
	response := mustRequest(t, http.MethodGet, httpServer.URL+"/sessions/"+created.SessionID, nil, bearer(owner))
	var status session.Response
	if response.StatusCode != 200 {
		t.Fatalf("GET status=%d", response.StatusCode)
	}
	mustDecode(t, response.Body, &status)
	response.Body.Close()
	if status.Media.RawVideoTrack == nil || status.Media.VideoSenderActive {
		t.Fatal("send-only must have ingress and no return sender")
	}
	select {
	case <-returned:
		t.Fatal("server returned unsolicited video")
	default:
	}
	if !strings.Contains(logs.text(), "created WebRTC answer") || !strings.Contains(logs.text(), "received WebRTC video track") || strings.Contains(logs.text(), "level=ERROR") {
		t.Fatal("missing ingress logs or application error")
	}
	response = mustRequest(t, http.MethodDelete, httpServer.URL+"/sessions/"+created.SessionID, nil, bearer(owner))
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatalf("DELETE status=%d", response.StatusCode)
	}
	response = mustRequest(t, http.MethodGet, httpServer.URL+"/sessions/"+created.SessionID, nil, bearer(owner))
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatalf("GET deleted status=%d", response.StatusCode)
	}
	t.Log("POST=201 signaling=connected GET=200 processed_video>0 return_video=0 DELETE=204 GET_deleted=404; matching answer/ingress logs and no ERROR")
}
