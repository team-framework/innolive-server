package streaming

import (
	"context"
	"errors"

	"inno-live-server/internal/auth"

	"github.com/google/uuid"
)

// ConnectionChecker는 사용자의 플랫폼 계정이 연결돼 있는지 알려 준다. 선택 구현이라
// 호출자는 타입 단언으로 지원 여부를 가린다 — 동시 송출 제안(#333)은 추가할
// 플랫폼 계정이 연결돼 있을 때만 건다.
type ConnectionChecker interface {
	Connected(ctx context.Context, userID uuid.UUID) (bool, error)
}

// AccountResolver는 이 요청이 쓸 송출 연결 ID를 확정한다(#390). ctx에 실린 연결
// ID(auth.WithStreamingAccount)가 있으면 그 연결이 사용자 것인지 확인하고, 없으면
// 연결이 하나일 때만 그것을 고른다. 유튜브 채널이 여러 개인데 지정이 없으면
// auth.ErrStreamingAccountSelectionRequired다. 플랫폼 채널 ID도 함께 돌려준다 — 한
// 채널은 동시에 한 InnoLive 계정만 송출하므로 준비 때 채널을 선점한다(#406).
type AccountResolver interface {
	ResolveAccount(ctx context.Context, userID uuid.UUID) (accountID uuid.UUID, channelID string, err error)
}

func (p *YouTubeProvider) ResolveAccount(ctx context.Context, userID uuid.UUID) (uuid.UUID, string, error) {
	account, err := p.store.Get(ctx, userID, auth.StreamingProviderYouTube)
	if errors.Is(err, auth.ErrStreamingAccountNotFound) {
		return uuid.Nil, "", auth.ErrStreamingNotConnected
	}
	if err != nil {
		return uuid.Nil, "", err
	}
	return account.ID, account.ChannelID, nil
}

func (p *YouTubeProvider) Connected(ctx context.Context, userID uuid.UUID) (bool, error) {
	return tokenConnected(ctx, p.tokens, userID)
}

func (p *ChzzkProvider) Connected(ctx context.Context, userID uuid.UUID) (bool, error) {
	return tokenConnected(ctx, p.tokens, userID)
}

// tokenConnected는 access token을 받을 수 있으면 연결된 것으로 본다. 재연결이
// 필요한 계정은 방송을 열 수 없으므로 연결되지 않은 것으로 친다. 유튜브 채널이
// 여러 개라 하나를 고르라는 답은 연결된 채널이 있다는 뜻이다(#390).
func tokenConnected(ctx context.Context, tokens AccessTokenProvider, userID uuid.UUID) (bool, error) {
	_, err := tokens.AccessToken(ctx, userID)
	switch {
	case err == nil, errors.Is(err, auth.ErrStreamingAccountSelectionRequired):
		return true, nil
	case errors.Is(err, auth.ErrStreamingNotConnected), errors.Is(err, auth.ErrStreamingReconnectRequired):
		return false, nil
	default:
		return false, err
	}
}
