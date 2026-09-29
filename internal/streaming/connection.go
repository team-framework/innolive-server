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

func (p *YouTubeProvider) Connected(ctx context.Context, userID uuid.UUID) (bool, error) {
	return tokenConnected(ctx, p.tokens, userID)
}

func (p *ChzzkProvider) Connected(ctx context.Context, userID uuid.UUID) (bool, error) {
	return tokenConnected(ctx, p.tokens, userID)
}

// tokenConnected는 access token을 받을 수 있으면 연결된 것으로 본다. 재연결이
// 필요한 계정은 방송을 열 수 없으므로 연결되지 않은 것으로 친다.
func tokenConnected(ctx context.Context, tokens AccessTokenProvider, userID uuid.UUID) (bool, error) {
	_, err := tokens.AccessToken(ctx, userID)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, auth.ErrStreamingNotConnected), errors.Is(err, auth.ErrStreamingReconnectRequired):
		return false, nil
	default:
		return false, err
	}
}
