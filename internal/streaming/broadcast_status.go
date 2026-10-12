package streaming

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// BroadcastStatusChecker는 플랫폼 방송이 아직 진행 중인지 알려 준다(#360). 사용자가
// 플랫폼 스튜디오에서 방송을 끝내면 서버는 알 수 없으므로 주기적으로 묻는다.
// 선택 구현이다.
type BroadcastStatusChecker interface {
	BroadcastEnded(ctx context.Context, userID uuid.UUID, broadcastID string) (bool, error)
}

// BroadcastEnded는 liveBroadcasts.list(id)로 방송의 lifeCycleStatus를 읽는다.
// complete·revoked이거나 방송이 사라졌으면 끝난 것이다. 쿼터 1유닛.
func (p *YouTubeProvider) BroadcastEnded(ctx context.Context, userID uuid.UUID, broadcastID string) (bool, error) {
	accessToken, err := p.tokens.AccessToken(ctx, userID)
	if err != nil {
		return false, err
	}
	var response struct {
		Items []struct {
			Status struct {
				LifeCycleStatus string `json:"lifeCycleStatus"`
			} `json:"status"`
		} `json:"items"`
	}
	path := p.apiBase + "/liveBroadcasts?part=status&id=" + broadcastID
	if err := p.do(ctx, accessToken, "GET", path, "", nil, &response); err != nil {
		return false, fmt.Errorf("read broadcast status: %w", err)
	}
	if len(response.Items) == 0 {
		return true, nil
	}
	switch response.Items[0].Status.LifeCycleStatus {
	case "complete", "revoked":
		return true, nil
	default:
		return false, nil
	}
}

// BroadcastTimesReader는 플랫폼이 기록한 방송의 실제 시작·종료 시각을 읽는다(#437).
// 서버가 영상을 받지 않는 직접 송출의 사용 기록을 플랫폼 기록과 대조하는 데 쓴다.
// 선택 구현이다.
type BroadcastTimesReader interface {
	BroadcastTimes(ctx context.Context, userID uuid.UUID, broadcastID string) (started, ended *time.Time, err error)
}

// BroadcastTimes는 liveBroadcasts.list(id)의 snippet.actualStartTime·actualEndTime을
// 읽는다. 아직 시작·종료되지 않았거나 방송이 사라졌으면 해당 값은 nil이다. 쿼터 1유닛.
func (p *YouTubeProvider) BroadcastTimes(ctx context.Context, userID uuid.UUID, broadcastID string) (*time.Time, *time.Time, error) {
	accessToken, err := p.tokens.AccessToken(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	var response struct {
		Items []struct {
			Snippet struct {
				ActualStartTime *time.Time `json:"actualStartTime"`
				ActualEndTime   *time.Time `json:"actualEndTime"`
			} `json:"snippet"`
		} `json:"items"`
	}
	path := p.apiBase + "/liveBroadcasts?part=snippet&id=" + url.QueryEscape(broadcastID)
	if err := p.do(ctx, accessToken, "GET", path, "", nil, &response); err != nil {
		return nil, nil, fmt.Errorf("read broadcast times: %w", err)
	}
	if len(response.Items) == 0 {
		return nil, nil, nil
	}
	return response.Items[0].Snippet.ActualStartTime, response.Items[0].Snippet.ActualEndTime, nil
}
