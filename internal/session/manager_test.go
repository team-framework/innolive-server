package session

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/media"
	"inno-live-server/internal/metrics"
)

func newTestManager(t *testing.T, maxSessions int) *Manager {
	t.Helper()
	cfg := config.Config{
		PrivacyMode:    config.PrivacyModeBypass,
		FFmpegPath:     "ffmpeg",
		UDPPortMin:     42000,
		UDPPortMax:     42100,
		FrameQueueSize: 2,
		MaxSessions:    maxSessions,
	}
	manager, err := NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	return manager
}

func TestCreateEnforcesMaxSessions(t *testing.T) {
	manager := newTestManager(t, 2)
	first, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Create(nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Create(nil); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("third Create() error = %v, want ErrCapacityExceeded", err)
	}
	if active, limit := manager.Capacity(); active != 2 || limit != 2 {
		t.Fatalf("Capacity() = (%d, %d), want (2, 2)", active, limit)
	}
	if first.Response().Media.AIFallbackActive {
		t.Fatal("AIFallbackActive should default to false before any track exists")
	}

	if err := manager.Delete(first.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Create(nil); err != nil {
		t.Fatalf("Create() after Delete() error = %v, want slot freed", err)
	}
}

func TestCreateUnlimitedWhenMaxSessionsZero(t *testing.T) {
	manager := newTestManager(t, 0)
	for i := 0; i < 3; i++ {
		if _, _, err := manager.Create(nil); err != nil {
			t.Fatalf("Create() #%d error = %v", i, err)
		}
	}
}

func TestConcurrentCreateNeverExceedsLimit(t *testing.T) {
	const limit = 4
	const attempts = 16
	manager := newTestManager(t, limit)

	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	rejected := 0
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := manager.Create(nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, ErrCapacityExceeded):
				rejected++
			default:
				t.Errorf("Create() unexpected error = %v", err)
			}
		}()
	}
	wg.Wait()

	if created != limit {
		t.Fatalf("created = %d, want exactly %d", created, limit)
	}
	if rejected != attempts-limit {
		t.Fatalf("rejected = %d, want %d", rejected, attempts-limit)
	}
	if sessions := manager.List(); len(sessions) != limit {
		t.Fatalf("List() length = %d, want %d", len(sessions), limit)
	}
}

func TestGetAndDeleteNotFound(t *testing.T) {
	manager := newTestManager(t, 0)
	if _, err := manager.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
	if err := manager.Delete("missing", "test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete() error = %v, want ErrNotFound", err)
	}
}

func TestReapsUnnegotiatedSessions(t *testing.T) {
	cfg := config.Config{
		PrivacyMode:        config.PrivacyModeBypass,
		FFmpegPath:         "ffmpeg",
		UDPPortMin:         42000,
		UDPPortMax:         42100,
		FrameQueueSize:     2,
		NegotiationTimeout: 100 * time.Millisecond,
	}
	manager, err := NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)

	created, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := manager.Get(created.ID); errors.Is(err, ErrNotFound) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("unnegotiated session was not reaped within the timeout window")
}

// TestCreateWithYoutubeKeyOwnsSessionScopedEgress: 전역 키가 설정되면 egress는
// 세션 생성 시점에 세션 수명으로 만들어지고(#84 — 트랙 수명이 아니라),
// 응답 StreamState가 egress 상태를 반영하며, 세션 삭제 시 함께 종료돼야 한다.
func TestCreateWithYoutubeKeyOwnsSessionScopedEgress(t *testing.T) {
	cfg := config.Config{
		PrivacyMode:      config.PrivacyModeBypass,
		FFmpegPath:       "ffmpeg",
		UDPPortMin:       42000,
		UDPPortMax:       42100,
		FrameQueueSize:   2,
		YoutubeStreamKey: "test-stream-key",
	}
	manager, err := NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)

	created, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.egress == nil {
		t.Fatal("session with YoutubeStreamKey must own an egress at creation")
	}
	if created.egressCancel == nil {
		t.Fatal("session egress must have a dedicated cancel")
	}

	stream := created.Response().Stream
	if stream.Status != string(media.EgressPhaseIdle) {
		t.Fatalf("initial Stream.Status = %q, want %q", stream.Status, media.EgressPhaseIdle)
	}
	if stream.TargetURL == nil {
		t.Fatal("Stream.TargetURL must be populated when egress exists")
	}
	if strings.Contains(*stream.TargetURL, "test-stream-key") {
		t.Fatalf("Stream.TargetURL leaks the stream key: %q", *stream.TargetURL)
	}
	if !strings.HasSuffix(*stream.TargetURL, "/****") {
		t.Fatalf("Stream.TargetURL is not masked: %q", *stream.TargetURL)
	}

	if err := manager.Delete(created.ID, "test"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if created.egress.Status().Phase == media.EgressPhaseStopped {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("egress did not stop after session delete")
}

// TestCreateWithoutYoutubeKeyKeepsIdleStream: 키가 없으면 egress를 만들지 않고
// StreamState는 종전 기본값(idle)을 유지한다 — 기존 동작 보존 검증.
func TestCreateWithoutYoutubeKeyKeepsIdleStream(t *testing.T) {
	manager := newTestManager(t, 0)
	created, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.egress != nil || created.egressCancel != nil {
		t.Fatal("session without YoutubeStreamKey must not own an egress")
	}
	stream := created.Response().Stream
	if stream.Status != "idle" {
		t.Fatalf("Stream.Status = %q, want %q", stream.Status, "idle")
	}
	if stream.TargetURL != nil {
		t.Fatalf("Stream.TargetURL = %v, want nil", *stream.TargetURL)
	}
}
