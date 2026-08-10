package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// StreamingAccountSummary는 연결 목록 조회 API의 응답 항목이다. 플랫폼 중립
// 형태라 치지직이 붙어도 배열 항목만 늘어난다(#88).
type StreamingAccountSummary struct {
	Provider     StreamingProvider `json:"provider"`
	ChannelID    string            `json:"channel_id"`
	ChannelTitle string            `json:"channel_title"`
	ConnectedAt  time.Time         `json:"connected_at"`
	// ReconnectRequired는 저장된 표식(reconnect_required_at)과 refresh token
	// 만료 시각만으로 판별한다 — 조회마다 플랫폼 API를 부르지 않는다.
	ReconnectRequired bool `json:"reconnect_required"`
}

// StreamingAccountService는 송출 계정 연결의 조회·해제를 담당한다.
type StreamingAccountService struct {
	store StreamingAccountStore
	users UserStatusChecker
	now   func() time.Time
}

func NewStreamingAccountService(store StreamingAccountStore, users UserStatusChecker) (*StreamingAccountService, error) {
	if store == nil || users == nil {
		return nil, errors.New("streaming account service dependencies must not be nil")
	}
	return &StreamingAccountService{
		store: store,
		users: users,
		now:   func() time.Time { return time.Now().UTC() },
	}, nil
}

// List는 사용자의 플랫폼 연결 목록을 돌려준다. 연결이 없으면 빈 슬라이스다
// (404가 아니라 빈 배열 — 이슈 #88 계약).
func (s *StreamingAccountService) List(ctx context.Context, userID uuid.UUID) ([]StreamingAccountSummary, error) {
	if err := s.ensureActive(ctx, userID); err != nil {
		return nil, err
	}
	accounts, err := s.store.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	// make로 시작해 JSON이 null이 아니라 []로 직렬화되게 한다.
	summaries := make([]StreamingAccountSummary, 0, len(accounts))
	for _, account := range accounts {
		summary := StreamingAccountSummary{
			Provider:          account.Provider,
			ChannelID:         account.ChannelID,
			ConnectedAt:       account.ConnectedAt,
			ReconnectRequired: s.reconnectRequired(account),
		}
		if account.ChannelTitle != nil {
			summary.ChannelTitle = *account.ChannelTitle
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

func (s *StreamingAccountService) reconnectRequired(account StreamingAccount) bool {
	if account.ReconnectRequiredAt != nil {
		return true
	}
	// Testing 게시 상태 시절 발급된 토큰의 만료(실측 7일). 만료가 지났으면
	// 갱신 시도가 실패할 것이 확정적이므로 시도 전에 재연결로 안내한다.
	if account.RefreshTokenExpiresAt != nil && !s.now().Before(*account.RefreshTokenExpiresAt) {
		return true
	}
	return false
}

func (s *StreamingAccountService) ensureActive(ctx context.Context, userID uuid.UUID) error {
	status, err := s.users.UserStatus(ctx, userID)
	if err != nil {
		return err
	}
	if status != UserStatusActive {
		return ErrUserInactive
	}
	return nil
}
