package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// oauthStateStore는 OAuth authorize 왕복의 state를 보관한다. 콜백은 브라우저
// 리다이렉트로 도착해 Authorization 헤더를 실을 수 없으므로, state가 콜백
// 요청을 시작 사용자와 잇는 유일한 바인딩이다. 단일 인스턴스 배포 전제의
// 인메모리 저장이며, 서버 재시작 시 진행 중이던 연결 플로우만 무효화된다
// (사용자가 연결을 다시 시작하면 복구된다).
type oauthStateStore struct {
	mu      sync.Mutex
	entries map[string]oauthStateEntry
	ttl     time.Duration
	now     func() time.Time
}

type oauthStateEntry struct {
	userID    uuid.UUID
	expiresAt time.Time
}

func newOAuthStateStore(ttl time.Duration) *oauthStateStore {
	return &oauthStateStore{
		entries: make(map[string]oauthStateEntry),
		ttl:     ttl,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// Issue는 사용자에 바인딩된 1회용 state를 발급한다.
func (s *oauthStateStore) Issue(userID uuid.UUID) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	state := hex.EncodeToString(raw)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	// 발급 시점마다 만료 항목을 청소한다. 진행 중인 연결 플로우 수만큼만
	// 쌓이는 맵이라 순회 비용은 무시할 수 있다.
	for key, entry := range s.entries {
		if !now.Before(entry.expiresAt) {
			delete(s.entries, key)
		}
	}
	s.entries[state] = oauthStateEntry{userID: userID, expiresAt: now.Add(s.ttl)}
	return state, nil
}

// Consume은 state를 검증하고 바인딩된 사용자를 돌려준다. 성공·실패와 무관하게
// 조회된 state는 즉시 제거된다(1회용 — 재사용은 CSRF 재시도 신호다).
func (s *oauthStateStore) Consume(state string) (uuid.UUID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[state]
	if !ok {
		return uuid.Nil, false
	}
	delete(s.entries, state)
	if !s.now().Before(entry.expiresAt) {
		return uuid.Nil, false
	}
	return entry.userID, true
}
