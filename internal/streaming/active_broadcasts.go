package streaming

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// ActiveBroadcastChecker는 채널에 이미 라이브 중인 방송이 몇 개인지 알려 준다
// (#361). 다른 도구(OBS 등)로 방송 중인 채널에서 InnoLive가 방송을 하나 더 열기
// 전에 사용자에게 확인받는 데 쓴다. 선택 구현이다.
type ActiveBroadcastChecker interface {
	ActiveBroadcasts(ctx context.Context, userID uuid.UUID) ([]string, error)
}

// ActiveBroadcasts는 liveBroadcasts.list(broadcastStatus=active)로 채널의 라이브
// 방송 id를 돌려준다. 쿼터 1유닛.
func (p *YouTubeProvider) ActiveBroadcasts(ctx context.Context, userID uuid.UUID) ([]string, error) {
	accessToken, err := p.tokens.AccessToken(ctx, userID)
	if err != nil {
		return nil, err
	}
	var response struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	path := p.apiBase + "/liveBroadcasts?part=id&broadcastStatus=active&broadcastType=all&maxResults=5"
	if err := p.do(ctx, accessToken, "GET", path, "", nil, &response); err != nil {
		return nil, fmt.Errorf("list active broadcasts: %w", err)
	}
	ids := make([]string, 0, len(response.Items))
	for _, item := range response.Items {
		ids = append(ids, item.ID)
	}
	return ids, nil
}
