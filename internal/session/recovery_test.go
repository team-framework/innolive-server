package session

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
)

func TestPeerRecoveryAcceptsOnlyConfiguredICERestartOffers(t *testing.T) {
	manager := newTestManager(t, 0)
	liveSession, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if status := liveSession.Response().Peer.RecoveryStatus; status != PeerRecoveryStatusIdle {
		t.Fatalf("initial RecoveryStatus = %q, want %q", status, PeerRecoveryStatusIdle)
	}
	liveSession.mu.Lock()
	liveSession.wasConnected = true
	liveSession.mu.Unlock()

	for attempt := 1; attempt <= defaultWebRTCRecoveryMaxAttempts; attempt++ {
		if err := manager.registerRecoveryOffer(liveSession); err != nil {
			t.Fatalf("registerRecoveryOffer attempt %d: %v", attempt, err)
		}
	}
	if err := manager.registerRecoveryOffer(liveSession); !errors.Is(err, ErrRecoveryAttemptsExhausted) {
		t.Fatalf("eleventh registerRecoveryOffer error = %v, want ErrRecoveryAttemptsExhausted", err)
	}

	response := liveSession.Response()
	if response.Peer.RecoveryStatus != PeerRecoveryStatusRecovering {
		t.Fatalf("RecoveryStatus = %q, want %q", response.Peer.RecoveryStatus, PeerRecoveryStatusRecovering)
	}
	if response.Peer.ReconnectAttempts != defaultWebRTCRecoveryMaxAttempts {
		t.Fatalf("ReconnectAttempts = %d, want %d", response.Peer.ReconnectAttempts, defaultWebRTCRecoveryMaxAttempts)
	}
	if response.Peer.RecoveryDeadline == nil {
		t.Fatal("RecoveryDeadline must be visible while recovering")
	}
}

func TestPeerRecoveryAcceptsRestartBeforeServerConnectedCallback(t *testing.T) {
	manager := newTestManager(t, 0)
	liveSession, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}

	// 실제 연결에서는 클라이언트가 connected를 먼저 보고 restart offer를 보낸
	// 뒤 서버의 connected 콜백이 wasConnected를 기록할 수 있다. 이 기록이
	// 늦어도 유효한 ICE restart offer를 session not found로 거부하면 안 된다.
	if err := manager.registerRecoveryOffer(liveSession); err != nil {
		t.Fatalf("registerRecoveryOffer() error = %v", err)
	}
	response := liveSession.Response()
	if response.Peer.RecoveryStatus != PeerRecoveryStatusRecovering {
		t.Fatalf("RecoveryStatus = %q, want %q", response.Peer.RecoveryStatus, PeerRecoveryStatusRecovering)
	}
	if response.Peer.ReconnectAttempts != 1 {
		t.Fatalf("ReconnectAttempts = %d, want 1", response.Peer.ReconnectAttempts)
	}
}

func TestPeerRecoveryDeadlineDeletesSession(t *testing.T) {
	cfg := config.Config{
		PrivacyMode:            config.PrivacyModeBypass,
		FFmpegPath:             "ffmpeg",
		UDPPortMin:             44000,
		UDPPortMax:             44100,
		FrameQueueSize:         2,
		WebRTCRecoveryWindow:   25 * time.Millisecond,
		WebRTCRecoveryAttempts: 1,
	}
	manager, err := NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	liveSession, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	liveSession.mu.Lock()
	liveSession.wasConnected = true
	liveSession.mu.Unlock()
	manager.beginRecovery(liveSession, 0, "test")

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := manager.Get(liveSession.ID); errors.Is(err, ErrNotFound) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("recovery deadline did not delete the session")
}

func TestICECandidateRejectsPreviousNegotiationGeneration(t *testing.T) {
	manager := newTestManager(t, 0)
	liveSession, ownerToken, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	currentID := uuid.NewString()
	liveSession.mu.Lock()
	liveSession.activeNegotiationID = currentID
	liveSession.mu.Unlock()

	_, err = manager.AddICECandidateWithNegotiation(liveSession.ID, ownerToken, uuid.NewString(), webrtc.ICECandidateInit{
		Candidate: "candidate:0 1 udp 1 127.0.0.1 9 typ host",
	})
	if !errors.Is(err, ErrStaleNegotiation) {
		t.Fatalf("old candidate error = %v, want ErrStaleNegotiation", err)
	}
}
