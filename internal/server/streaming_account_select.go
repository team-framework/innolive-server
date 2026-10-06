package server

import (
	"context"
	"net/http"
	"strings"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
)

// parseAccountID는 요청의 송출 연결 ID(account_id)를 읽는다. 비어 있으면 uuid.Nil이다.
func parseAccountID(raw string) (uuid.UUID, *apiError) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		invalid := badRequest("account_id must be a streaming account id.", map[string]any{"field": "account_id"})
		return uuid.Nil, &invalid
	}
	return id, nil
}

// streamingAccountContext는 이 방송이 쓸 송출 연결을 확정해 ctx에 싣는다(#390).
// 유튜브는 채널이 여러 개일 수 있어 요청의 account_id를 확인하고, 생략했으면 연결이
// 하나일 때만 그것을 쓴다. 서버가 채널을 대신 고르지 않는다 — 여러 개인데 생략하면
// 409 youtube_channel_required다. 치지직은 연결이 하나라 고를 것이 없다.
func (s *Server) streamingAccountContext(ctx context.Context, userID uuid.UUID, providerName auth.StreamingProvider, requested uuid.UUID) (context.Context, *apiError) {
	if providerName != auth.StreamingProviderYouTube {
		return ctx, nil
	}
	resolver, ok := s.streaming[providerName].(streaming.AccountResolver)
	if !ok {
		return ctx, nil
	}
	accountID, _, err := resolver.ResolveAccount(auth.WithStreamingAccount(ctx, requested), userID)
	if err != nil {
		return ctx, s.prepareError(err, "", providerName)
	}
	return auth.WithStreamingAccount(ctx, accountID), nil
}

// broadcastChannel은 이 준비가 선점할 플랫폼 채널 키다(#406). 유튜브만 채널을
// 선점한다 — 연결 ID는 ctx에 이미 실려 있다(streamingAccountContext·전환 경로).
func (s *Server) broadcastChannel(ctx context.Context, userID uuid.UUID, providerName auth.StreamingProvider) (string, *apiError) {
	if providerName != auth.StreamingProviderYouTube {
		return "", nil
	}
	resolver, ok := s.streaming[providerName].(streaming.AccountResolver)
	if !ok {
		return "", nil
	}
	_, channelID, err := resolver.ResolveAccount(ctx, userID)
	if err != nil {
		return "", s.prepareError(err, "", providerName)
	}
	return string(providerName) + "/" + channelID, nil
}

func channelInUseError(providerName auth.StreamingProvider) *apiError {
	return &apiError{Status: http.StatusConflict, Code: "channel_in_use_by_other_account",
		Message: "Another InnoLive account is broadcasting to this channel. Try again after that broadcast ends.",
		Details: map[string]any{"provider": providerName}}
}

func youtubeChannelRequiredError(providerName auth.StreamingProvider) *apiError {
	return &apiError{Status: http.StatusConflict, Code: "youtube_channel_required",
		Message: "Several YouTube channels are connected. Choose the channel to stream to.",
		Details: map[string]any{"provider": providerName, "field": "account_id"}}
}
