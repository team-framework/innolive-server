package streaming

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"inno-live-server/internal/auth"

	"github.com/google/uuid"
)

// 방송을 만든 뒤 스트림 연결(bind)이 실패하면 만든 방송이 채널에 고아로 남는다.
// 준비는 실패로 끝나므로 방송을 지워야 한다(P1-11).
func TestPrepareDeletesBroadcastWhenBindFails(t *testing.T) {
	stub := &youtubeAPIStub{bindFailures: 1, bindFailStatus: http.StatusInternalServerError, bindFailBody: `{"error":{"code":500,"message":"backend error"}}`}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID)
	provider := testProviderWith(t, stub, store)

	if _, err := provider.Prepare(context.Background(), userID, PrepareOptions{MadeForKids: boolPtr(false)}); err == nil {
		t.Fatal("Prepare succeeded although bind failed")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.deletes != 1 || !strings.Contains(stub.deleteQuery, "id=broadcast-id-1") {
		t.Fatalf("deletes = %d (%q), want the unbound broadcast deleted", stub.deletes, stub.deleteQuery)
	}
}

// 저장해 둔 재사용 스트림을 사용자가 YouTube에서 지웠으면 bind가 liveStreamNotFound로
// 실패한다. 스트림을 한 번 새로 만들어 다시 묶어야 준비가 영구히 실패하지 않는다(P1-11).
func TestPrepareRecreatesDeletedReusableStream(t *testing.T) {
	stub := &youtubeAPIStub{}
	store := newMemoryStore()
	userID := uuid.New()
	connectedAccount(t, store, userID)
	provider := testProviderWith(t, stub, store)
	if _, err := provider.Prepare(context.Background(), userID, PrepareOptions{MadeForKids: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	stub.bindFailures = 1
	stub.bindFailStatus = http.StatusNotFound
	stub.bindFailBody = `{"error":{"code":404,"message":"Stream not found","errors":[{"reason":"liveStreamNotFound"}]}}`
	stub.mu.Unlock()

	prepared, err := provider.Prepare(context.Background(), userID, PrepareOptions{MadeForKids: boolPtr(false)})
	if err != nil {
		t.Fatalf("Prepare after the stream was deleted = %v", err)
	}
	if prepared.StreamID == "" {
		t.Fatalf("prepared = %+v", prepared)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.streamInserts != 2 || stub.binds != 3 || stub.deletes != 0 {
		t.Fatalf("stream inserts/binds/deletes = %d/%d/%d, want 2/3/0", stub.streamInserts, stub.binds, stub.deletes)
	}
	account, err := store.Get(context.Background(), userID, auth.StreamingProviderYouTube)
	if err != nil || account.StreamID == nil {
		t.Fatalf("stored stream = %v, %v", account.StreamID, err)
	}
}
