package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
)

type timesStreamingProvider struct {
	*stubStreamingProvider
	started, ended *time.Time
	err            error
}

func (p *timesStreamingProvider) BroadcastTimes(context.Context, uuid.UUID, string) (*time.Time, *time.Time, error) {
	return p.started, p.ended, p.err
}

type platformTimesCall struct {
	usageID, broadcastID string
	started, ended       *time.Time
}

type fakePlatformTimes struct {
	mu    sync.Mutex
	calls []platformTimesCall
}

func (f *fakePlatformTimes) RecordPlatformTimes(_ context.Context, usageID, broadcastID string, started, ended *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, platformTimesCall{usageID, broadcastID, started, ended})
	return nil
}

func (f *fakePlatformTimes) snapshot() []platformTimesCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]platformTimesCall(nil), f.calls...)
}

func runDirectBroadcastToStop(t *testing.T, youtube *timesStreamingProvider) *fakePlatformTimes {
	t.Helper()
	previous := platformTimesDelay
	platformTimesDelay = 0
	t.Cleanup(func() { platformTimesDelay = previous })
	server, _, app := newStreamTestApplicationWithServer(t, map[auth.StreamingProvider]streaming.Provider{auth.StreamingProviderYouTube: youtube})
	recorder := &fakePlatformTimes{}
	app.SetPlatformTimesStore(recorder)
	sessionID, ownerToken := onDeviceStreamSession(t, server.URL)
	prepareStream(t, server.URL, sessionID, ownerToken, `{}`)
	if response, payload := goLive(t, server.URL, sessionID, ownerToken); response.StatusCode != http.StatusOK {
		t.Fatalf("go live = %d (payload %v)", response.StatusCode, payload)
	}
	if response, payload := postStream(t, server.URL, sessionID, ownerToken, "stop", ""); response.StatusCode != http.StatusOK {
		t.Fatalf("stop = %d (payload %v)", response.StatusCode, payload)
	}
	return recorder
}

// TestDirectBroadcastRecordsPlatformTimes: 직접 송출이 끝나면 플랫폼의 실제 방송
// 시각을 읽어 그 송출 세대에 남긴다(#437).
func TestDirectBroadcastRecordsPlatformTimes(t *testing.T) {
	started := time.Date(2026, 10, 12, 8, 0, 5, 0, time.UTC)
	ended := started.Add(30 * time.Minute)
	youtube := &timesStreamingProvider{stubStreamingProvider: &stubStreamingProvider{prepared: streaming.PreparedBroadcast{
		Provider: auth.StreamingProviderYouTube, BroadcastID: "b1", IngestURL: "rtmps://a.rtmp.youtube.com/live2/secret-key",
	}}, started: &started, ended: &ended}
	recorder := runDirectBroadcastToStop(t, youtube)

	deadline := time.Now().Add(5 * time.Second)
	for len(recorder.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	calls := recorder.snapshot()
	if len(calls) != 1 {
		t.Fatalf("platform times calls = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.usageID == "" || call.broadcastID != "b1" || !call.started.Equal(started) || !call.ended.Equal(ended) {
		t.Fatalf("recorded = %+v", call)
	}
}

// TestDirectBroadcastPlatformTimesReadFailure: 플랫폼 조회가 실패하면 기록하지
// 않는다 — 방송·중지는 그대로 성공한다.
func TestDirectBroadcastPlatformTimesReadFailure(t *testing.T) {
	youtube := &timesStreamingProvider{stubStreamingProvider: &stubStreamingProvider{prepared: streaming.PreparedBroadcast{
		Provider: auth.StreamingProviderYouTube, BroadcastID: "b1", IngestURL: "rtmps://a.rtmp.youtube.com/live2/secret-key",
	}}, err: errors.New("platform unavailable")}
	recorder := runDirectBroadcastToStop(t, youtube)

	time.Sleep(200 * time.Millisecond)
	if calls := recorder.snapshot(); len(calls) != 0 {
		t.Fatalf("platform times recorded after a read failure: %+v", calls)
	}
}
