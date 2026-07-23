package session

import (
	"errors"
	"io"
	"log/slog"
	"sort"
	"testing"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"

	"github.com/pion/webrtc/v4"
)

func newAuthManager(t *testing.T) *Manager {
	t.Helper()
	cfg := config.Config{
		PrivacyMode:        config.PrivacyModeBypass,
		FFmpegPath:         "ffmpeg",
		UDPPortMin:         43000,
		UDPPortMax:         43100,
		FrameQueueSize:     2,
		RequireSessionAuth: true,
	}
	manager, err := NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	return manager
}

// TestSessionIDAndTokenAreRandom verifies session IDs and owner tokens are
// unique and unordered across many creations (no counter/sequence leakage).
func TestSessionIDAndTokenAreRandom(t *testing.T) {
	manager := newTestManager(t, 0)
	const n = 100
	ids := make([]string, 0, n)
	seenID := make(map[string]struct{}, n)
	seenToken := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		s, token, err := manager.Create(nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := seenID[s.ID]; dup {
			t.Fatalf("duplicate session_id: %s", s.ID)
		}
		if _, dup := seenToken[token]; dup {
			t.Fatalf("duplicate owner_token")
		}
		seenID[s.ID] = struct{}{}
		seenToken[token] = struct{}{}
		ids = append(ids, s.ID)
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	ordered := true
	for i := range ids {
		if ids[i] != sorted[i] {
			ordered = false
			break
		}
	}
	if ordered {
		t.Fatal("session IDs were generated in sorted order, suggesting a sequence rather than randomness")
	}
}

func TestVerifyOwnerToken(t *testing.T) {
	manager := newAuthManager(t)
	s, token, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s.verifyOwnerToken(token) {
		t.Fatal("correct token rejected")
	}
	if s.verifyOwnerToken(token + "x") {
		t.Fatal("tampered token accepted")
	}
	if s.verifyOwnerToken("") {
		t.Fatal("empty token accepted")
	}
}

func TestVerifyOwner(t *testing.T) {
	manager := newAuthManager(t)
	s, token, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := manager.VerifyOwner("missing", token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("VerifyOwner(missing) = %v, want ErrNotFound", err)
	}
	if _, err := manager.VerifyOwner(s.ID, "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("VerifyOwner(wrong token) = %v, want ErrUnauthorized", err)
	}
	if got, err := manager.VerifyOwner(s.ID, token); err != nil || got != s {
		t.Fatalf("VerifyOwner(correct) = (%v, %v), want the session", got, err)
	}
}

// TestVerifyOwnerAuthDisabled confirms that with auth disabled the token is not
// checked but the session must still exist.
func TestVerifyOwnerAuthDisabled(t *testing.T) {
	manager := newTestManager(t, 0) // RequireSessionAuth defaults to false
	s, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := manager.VerifyOwner(s.ID, "any-token"); err != nil || got != s {
		t.Fatalf("VerifyOwner with auth disabled = (%v, %v), want the session", got, err)
	}
	if _, err := manager.VerifyOwner("missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("VerifyOwner(missing) = %v, want ErrNotFound", err)
	}
}

// TestHijackOfferRejectedBeforeTouchingPeerConnection reproduces the issue: an
// attacker who knows a victim's session_id but not its owner token sends an
// offer. CreateAnswer must reject it before mutating the PeerConnection, so the
// victim's media path is untouched.
func TestHijackOfferRejectedBeforeTouchingPeerConnection(t *testing.T) {
	manager := newAuthManager(t)
	victim, _, err := manager.Create(nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = manager.CreateAnswer(victim.ID, "stolen-or-guessed", "v=0\r\nm=video")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("CreateAnswer with wrong token = %v, want ErrUnauthorized", err)
	}
	// The rejected offer must not have advanced the PeerConnection nor started
	// any track pipeline.
	if victim.PC.RemoteDescription() != nil {
		t.Fatal("attacker offer was applied to the victim PeerConnection")
	}
	victim.mu.RLock()
	rawTrackID := victim.rawTrackID
	trackCancel := victim.trackCancel
	victim.mu.RUnlock()
	if rawTrackID != "" || trackCancel != nil {
		t.Fatal("attacker offer started or replaced a track on the victim session")
	}

	// ICE candidates from the attacker are rejected the same way and never
	// queued onto the victim's PeerConnection.
	_, err = manager.AddICECandidate(victim.ID, "stolen-or-guessed", webrtc.ICECandidateInit{Candidate: "candidate:0 1 udp 1 1.2.3.4 5 typ host"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("AddICECandidate with wrong token = %v, want ErrUnauthorized", err)
	}
	victim.mu.RLock()
	pending := len(victim.pendingICE)
	victim.mu.RUnlock()
	if pending != 0 {
		t.Fatalf("attacker queued %d ICE candidates on the victim session", pending)
	}
}
