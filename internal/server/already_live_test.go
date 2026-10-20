package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
)

// activeCheckingProvider는 채널의 라이브 방송 id를 돌려주는 스텁이다.
type activeCheckingProvider struct {
	*stubStreamingProvider
	active []string
	quota  *streaming.QuotaMeter
}

func (p activeCheckingProvider) Quota() *streaming.QuotaMeter { return p.quota }

func (p activeCheckingProvider) ActiveBroadcasts(context.Context, uuid.UUID) ([]string, error) {
	return p.active, nil
}

// 기존 라이브와 쿼터 상태에 관계없이 YouTube 방송 준비를 호출한다.
func TestPrepareStreamAllowsConcurrentYouTubeBroadcast(t *testing.T) {
	for _, body := range []string{`{}`, `{"allow_concurrent":false}`, `{"allow_concurrent":true}`} {
		for _, lowQuota := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/lowQuota=%t", body, lowQuota), func(t *testing.T) {
				stub := &stubStreamingProvider{}
				meter := streaming.NewQuotaMeter()
				if lowQuota {
					for i := 0; i < 160; i++ {
						meter.Record(http.MethodPost)
					}
				}
				server, _, _ := newStreamTestApplicationWithServer(t, map[auth.StreamingProvider]streaming.Provider{
					auth.StreamingProviderYouTube: activeCheckingProvider{stubStreamingProvider: stub, active: []string{"obs-live", "innolive-own"}, quota: meter},
				})
				created, ownerToken := createTestSession(t, server.URL, nil)
				putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`)
				_, payload := prepareStream(t, server.URL, created.SessionID, ownerToken, body)
				if streamErrorCode(payload) == "channel_already_live" {
					t.Fatalf("prepare still asks for confirmation: %v", payload)
				}
				stub.mu.Lock()
				calls := stub.prepareCalls
				stub.mu.Unlock()
				if calls != 1 {
					t.Fatalf("prepare calls = %d, want 1 without confirmation; response=%v", calls, payload)
				}
			})
		}
	}
}
