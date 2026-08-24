package session

import "testing"

// TestDeferredServerTrickleCompletionKeepsOriginalNegotiationID는 initial
// gathering의 completion callback이 ICE restart offer 뒤에 실행되는 순서를
// 재현한다. callback이 Session의 현재 generation을 읽으면 restart ID로 잘못
// 표시되므로, callback 생성 시점의 initial ID가 유지돼야 한다.
func TestDeferredServerTrickleCompletionKeepsOriginalNegotiationID(t *testing.T) {
	liveSession := &Session{ID: "session-1"}
	initialID := "11111111-1111-4111-8111-111111111111"
	restartID := "22222222-2222-4222-8222-222222222222"
	initialMessages := make(chan LocalICECandidate, 1)
	restartMessages := make(chan LocalICECandidate, 1)

	// Pion이 initial gathering 중 callback을 이미 읽어 둔 뒤, restart offer가
	// 새 callback을 설치하는 상황이다. activeNegotiationID는 이미 restart ID로
	// 바뀌었어도, 서버가 클라이언트로 보내는 initial completion은 initial ID여야 한다.
	initialCallback := liveSession.localICECandidateDispatcher(initialID, func(candidate LocalICECandidate) {
		initialMessages <- candidate
	})
	liveSession.mu.Lock()
	liveSession.activeNegotiationID = restartID
	liveSession.mu.Unlock()
	_ = liveSession.localICECandidateDispatcher(restartID, func(candidate LocalICECandidate) {
		restartMessages <- candidate
	})

	initialCallback(nil)

	select {
	case message := <-initialMessages:
		if message.NegotiationID != initialID {
			t.Fatalf("deferred initial completion negotiation_id = %q, want %q", message.NegotiationID, initialID)
		}
		if message.Candidate != nil {
			t.Fatalf("deferred initial completion candidate = %q, want nil", *message.Candidate)
		}
	case <-restartMessages:
		t.Fatal("deferred initial completion was delivered to the restart generation")
	default:
		t.Fatal("deferred initial completion was not delivered")
	}
}
