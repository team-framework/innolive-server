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

// TestSessionIDAndTokenAreRandom: 여러 번 만들어도 세션 id와 소유자 토큰이 겹치지
// 않고 순서가 없다(카운터·순번 유출 없음).
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

// TestVerifyOwnerAuthDisabled: 인증을 끄면 토큰은 확인하지 않지만 세션은 있어야
// 한다.
func TestVerifyOwnerAuthDisabled(t *testing.T) {
	manager := newTestManager(t, 0) // RequireSessionAuth 기본값은 false
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

// TestHijackOfferRejectedBeforeTouchingPeerConnection은 이슈를 재현한다. 피해자의
// session_id는 알지만 소유자 토큰은 모르는 공격자가 offer를 보낸다. CreateAnswer는
// PeerConnection을 바꾸기 전에 거절해 피해자의 미디어 경로를 건드리지 않아야 한다.
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
	// 거절한 offer는 PeerConnection을 진행시키거나 트랙 파이프라인을 시작하지 않아야
	// 한다.
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

	// 공격자의 ICE candidate도 같은 방식으로 거절되고 피해자의 PeerConnection에 쌓이지
	// 않는다.
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
