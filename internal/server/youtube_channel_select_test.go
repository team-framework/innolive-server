package server

import (
	"context"
	"net/http"
	"testing"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
)

// channelSelectingProvider는 유튜브 채널 선택(#390)을 흉내 낸다. 채널이 여러 개인
// 사용자는 연결 ID 없이 고를 수 없고, 준비는 ctx에 실린 채널로 나간다.
type channelSelectingProvider struct {
	stubStreamingProvider
	channels        []uuid.UUID
	channelIDs      map[uuid.UUID]string
	preparedAccount uuid.UUID
}

func (p *channelSelectingProvider) ResolveAccount(ctx context.Context, _ uuid.UUID) (uuid.UUID, string, error) {
	if selected, ok := auth.StreamingAccountFromContext(ctx); ok {
		for _, id := range p.channels {
			if id == selected {
				return id, p.channelOf(id), nil
			}
		}
		return uuid.Nil, "", auth.ErrStreamingNotConnected
	}
	if len(p.channels) > 1 {
		return uuid.Nil, "", auth.ErrStreamingAccountSelectionRequired
	}
	return p.channels[0], p.channelOf(p.channels[0]), nil
}

// channelOf는 연결의 유튜브 채널 ID다. channelIDs가 없으면 연결마다 다른 채널이다.
func (p *channelSelectingProvider) channelOf(id uuid.UUID) string {
	if channel, ok := p.channelIDs[id]; ok {
		return channel
	}
	return "UC-" + id.String()
}

var _ streaming.AccountResolver = (*channelSelectingProvider)(nil)

func (p *channelSelectingProvider) Prepare(ctx context.Context, userID uuid.UUID, options streaming.PrepareOptions) (streaming.PreparedBroadcast, error) {
	prepared, err := p.stubStreamingProvider.Prepare(ctx, userID, options)
	p.preparedAccount, _ = auth.StreamingAccountFromContext(ctx)
	prepared.AccountID = p.preparedAccount
	return prepared, err
}

func TestPrepareStreamPinsChosenYouTubeChannel(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	provider := &channelSelectingProvider{
		stubStreamingProvider: stubStreamingProvider{prepared: streaming.PreparedBroadcast{
			Provider: auth.StreamingProviderYouTube, IngestURL: "rtmps://a.example/live2/secret", BroadcastID: "bid-1",
		}},
		channels: []uuid.UUID{first, second},
	}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{auth.StreamingProviderYouTube: provider})
	created, ownerToken := createTestSession(t, server.URL, nil)
	putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`)

	// 채널이 여러 개인데 고르지 않으면 서버가 대신 고르지 않는다.
	response, payload := prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	if response.StatusCode != http.StatusConflict || streamErrorCode(payload) != "youtube_channel_required" {
		t.Fatalf("unselected prepare = %d %v", response.StatusCode, payload)
	}
	if provider.prepareCalls != 0 {
		t.Fatal("unselected prepare reached the platform")
	}
	response, payload = prepareStream(t, server.URL, created.SessionID, ownerToken, `{"account_id":"nope"}`)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid account_id = %d %v", response.StatusCode, payload)
	}
	response, payload = prepareStream(t, server.URL, created.SessionID, ownerToken, `{"account_id":"`+uuid.NewString()+`"}`)
	if response.StatusCode != http.StatusConflict || streamErrorCode(payload) != "streaming_not_connected" {
		t.Fatalf("unknown account_id = %d %v", response.StatusCode, payload)
	}

	// 고른 채널로 준비하고, 그 방송의 이후 호출(여기서는 트랙이 없어 치우는 Stop)도
	// 같은 채널로 간다.
	prepareStream(t, server.URL, created.SessionID, ownerToken, `{"account_id":"`+second.String()+`"}`)
	if provider.preparedAccount != second {
		t.Fatalf("prepared on %v, want chosen channel %v", provider.preparedAccount, second)
	}
	if _, stopped := provider.stopped(); stopped.AccountID != second {
		t.Fatalf("discard went to %v, want pinned channel %v", stopped.AccountID, second)
	}
}

func TestPrepareStreamUsesOnlyYouTubeChannelWithoutAccountID(t *testing.T) {
	only := uuid.New()
	provider := &channelSelectingProvider{
		stubStreamingProvider: stubStreamingProvider{prepared: streaming.PreparedBroadcast{
			Provider: auth.StreamingProviderYouTube, IngestURL: "rtmps://a.example/live2/secret", BroadcastID: "bid-1",
		}},
		channels: []uuid.UUID{only},
	}
	server := newStreamTestApplication(t, map[auth.StreamingProvider]streaming.Provider{auth.StreamingProviderYouTube: provider})
	created, ownerToken := createTestSession(t, server.URL, nil)
	putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`)

	prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	if provider.preparedAccount != only {
		t.Fatalf("prepared on %v, want the only channel %v", provider.preparedAccount, only)
	}
}

// 한 유튜브 채널은 동시에 한 InnoLive 계정만 송출한다(#406). 다른 계정이 같은 채널로
// 준비·송출 중이면 플랫폼에 닿기 전에 409로 거절한다.
func TestPrepareStreamRejectsChannelUsedByOtherAccount(t *testing.T) {
	accountA, accountB := uuid.New(), uuid.New()
	provider := &channelSelectingProvider{
		stubStreamingProvider: stubStreamingProvider{prepared: streaming.PreparedBroadcast{
			Provider: auth.StreamingProviderYouTube, IngestURL: "rtmps://a.example/live2/secret", BroadcastID: "bid-1",
		}},
		channels:   []uuid.UUID{accountB},
		channelIDs: map[uuid.UUID]string{accountA: "UCshared", accountB: "UCshared"},
	}
	server, manager := newStreamTestApplicationWithManager(t, map[auth.StreamingProvider]streaming.Provider{auth.StreamingProviderYouTube: provider})
	holder, _, err := manager.CreateForUserWithResolution(uuid.New(), "youtube", "", "720p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.BeginBroadcastPrepareOnChannel(holder.ID, "youtube/UCshared", "youtube"); err != nil {
		t.Fatal(err)
	}
	created, ownerToken := createTestSession(t, server.URL, nil)
	putBroadcast(t, server.URL, created.SessionID, ownerToken, `{"made_for_kids":false}`)

	response, payload := prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	if response.StatusCode != http.StatusConflict || streamErrorCode(payload) != "channel_in_use_by_other_account" {
		t.Fatalf("shared channel prepare = %d %v", response.StatusCode, payload)
	}
	if provider.prepareCalls != 0 {
		t.Fatal("rejected prepare reached the platform")
	}

	// 다른 계정의 방송이 끝나면 준비할 수 있다.
	manager.ResetBroadcastPreparation(holder.ID, "youtube")
	prepareStream(t, server.URL, created.SessionID, ownerToken, `{}`)
	if provider.prepareCalls != 1 {
		t.Fatalf("prepare calls = %d, want 1 after the channel is free", provider.prepareCalls)
	}
}
