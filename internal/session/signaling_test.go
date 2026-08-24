package session

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// TestQueuedPionCandidateKeepsOriginalNegotiationID는 Pion이 initial candidate를
// 내부 queue에 두었다가 ICE restart offer 뒤에 callback으로 넘기는 경로를 재현한다.
// callback을 generation별로 교체하면 Pion은 새 handler를 읽어 old candidate를 restart
// generation으로 잘못 표기한다. 실제 Pion candidate의 ufrag로 route를 찾으면 initial
// generation으로 전달된다.
func TestQueuedPionCandidateKeepsOriginalNegotiationID(t *testing.T) {
	queuedCandidate := gatherPionLocalCandidate(t)
	initialUfrag := localCandidateUsernameFragment(queuedCandidate.ToJSON().Candidate)
	if initialUfrag == "" {
		t.Fatalf("Pion candidate has no ufrag: %q", queuedCandidate.ToJSON().Candidate)
	}

	liveSession := &Session{ID: "session-1"}
	initialID := "11111111-1111-4111-8111-111111111111"
	restartID := "22222222-2222-4222-8222-222222222222"
	initialMessages := make(chan LocalICECandidate, 1)
	restartMessages := make(chan LocalICECandidate, 1)

	if err := liveSession.registerLocalCandidateRoute("a=ice-ufrag:"+initialUfrag+"\r\n", initialID, func(candidate LocalICECandidate) {
		initialMessages <- candidate
	}); err != nil {
		t.Fatalf("register initial route: %v", err)
	}
	if err := liveSession.registerLocalCandidateRoute("a=ice-ufrag:restart-ufrag\r\n", restartID, func(candidate LocalICECandidate) {
		restartMessages <- candidate
	}); err != nil {
		t.Fatalf("register restart route: %v", err)
	}

	// Pion queue에서 꺼낸 initial candidate가 restart route 등록 뒤에 도착한다.
	liveSession.dispatchLocalICECandidate(queuedCandidate)

	select {
	case message := <-initialMessages:
		if message.NegotiationID != initialID {
			t.Fatalf("queued initial candidate negotiation_id = %q, want %q", message.NegotiationID, initialID)
		}
		if message.Candidate == nil {
			t.Fatal("queued initial candidate must not be serialized as end-of-candidates")
		}
	case <-restartMessages:
		t.Fatal("queued initial candidate was relabeled as the restart generation")
	case <-time.After(time.Second):
		t.Fatal("queued initial candidate was not delivered")
	}

	// Pion의 nil 완료 신호에는 ufrag가 없으므로, 이전 gathering completion을 새
	// generation의 완료로 잘못 보내지 않도록 서버는 원격에 전달하지 않는다.
	liveSession.dispatchLocalICECandidate(nil)
	select {
	case message := <-initialMessages:
		t.Fatalf("end-of-candidates was sent to initial generation: %+v", message)
	case message := <-restartMessages:
		t.Fatalf("end-of-candidates was sent to restart generation: %+v", message)
	default:
	}
}

func TestLocalICEUsernameFragments(t *testing.T) {
	fragments := localICEUsernameFragments("v=0\r\na=ice-ufrag:session\r\na=ice-ufrag:video\r\na=ice-ufrag:session\r\n")
	if len(fragments) != 2 || fragments[0] != "session" || fragments[1] != "video" {
		t.Fatalf("local ICE username fragments = %v, want [session video]", fragments)
	}
}

func gatherPionLocalCandidate(t *testing.T) *webrtc.ICECandidate {
	t.Helper()
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = peerConnection.Close()
	})

	candidates := make(chan *webrtc.ICECandidate, 1)
	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		select {
		case candidates <- candidate:
		default:
		}
	})
	if _, err := peerConnection.CreateDataChannel("candidate-generation-test", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := peerConnection.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case candidate := <-candidates:
		return candidate
	case <-time.After(5 * time.Second):
		t.Fatal("Pion did not gather a local candidate")
		return nil
	}
}
