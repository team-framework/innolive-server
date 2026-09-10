package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// StreamingDisconnectHooks는 연결 해제 시 수행할 플랫폼별 정리 동작이다.
// 훅이 없는 플랫폼(정리할 것이 없는 경우)은 해당 단계를 건너뛴다.
type StreamingDisconnectHooks struct {
	// CleanupResources는 플랫폼에 만들어 둔 리소스(재사용 스트림 등)를
	// 삭제한다 — DB 행만 지우면 사용자 채널에 고아 리소스가 누적된다.
	CleanupResources func(ctx context.Context, account StreamingAccount) error
	// RevokeToken은 플랫폼 쪽 권한 부여를 취소한다. 인자는 refresh token
	// 평문이다.
	RevokeToken func(ctx context.Context, refreshToken string) error
}

// StreamingAccountService는 송출 계정 연결의 조회·해제를 담당한다.
type StreamingAccountService struct {
	store  StreamingAccountStore
	users  UserStatusChecker
	cipher *ProviderTokenCipher
	hooks  map[StreamingProvider]StreamingDisconnectHooks
	logger *slog.Logger
	now    func() time.Time
	gate   interface {
		BeginOperation(uuid.UUID) (func(), bool)
	}
}

// NewStreamingAccountService를 만든다. cipher와 hooks는 해제 시 플랫폼 정리
// (토큰 복호화·revoke)에만 쓰이므로 nil이어도 조회·행 삭제는 동작한다.
func NewStreamingAccountService(store StreamingAccountStore, users UserStatusChecker, cipher *ProviderTokenCipher, hooks map[StreamingProvider]StreamingDisconnectHooks, logger *slog.Logger) (*StreamingAccountService, error) {
	if store == nil || users == nil {
		return nil, errors.New("streaming account service dependencies must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &StreamingAccountService{
		store:  store,
		users:  users,
		cipher: cipher,
		hooks:  hooks,
		logger: logger,
		now:    func() time.Time { return time.Now().UTC() },
	}, nil
}

// SetUserOperationGate prevents a new connect/disconnect request from racing
// with account withdrawal. The withdrawal cleanup method intentionally bypasses
// this gate because it already owns the user's exclusive slot.
func (s *StreamingAccountService) SetUserOperationGate(gate interface {
	BeginOperation(uuid.UUID) (func(), bool)
}) {
	if s != nil {
		s.gate = gate
	}
}

// Disconnect는 연결을 해제한다. 세 단계를 순서대로 수행한다(#88):
// ①플랫폼 리소스 삭제 → ②플랫폼 권한 취소 → ③DB 행 삭제. 토큰을 먼저
// 폐기하면 ①을 못 하므로 순서를 바꾸면 안 되고, ①·②가 실패해도 ③은
// 수행한다 — 이미 토큰이 무효화된 연결을 해제하는 것이 정상 시나리오다.
func (s *StreamingAccountService) Disconnect(ctx context.Context, userID uuid.UUID, provider StreamingProvider) error {
	release, admitted := s.beginOperation(userID)
	if !admitted {
		return ErrWithdrawalInProgress
	}
	defer release()

	if err := s.ensureActive(ctx, userID); err != nil {
		return err
	}
	account, err := s.store.Get(ctx, userID, provider)
	if err != nil {
		return err
	}
	hooks := s.hooks[provider]
	if hooks.CleanupResources != nil {
		if err := hooks.CleanupResources(ctx, account); err != nil {
			s.logger.Warn("streaming resource cleanup failed; continuing disconnect",
				"provider", provider, "user_id", userID, "error", err)
		}
	}
	if hooks.RevokeToken != nil && s.cipher != nil && len(account.RefreshTokenCiphertext) > 0 {
		refreshToken, err := s.cipher.Decrypt(account.RefreshTokenCiphertext, account.TokenKeyVersion)
		if err != nil {
			s.logger.Warn("streaming refresh token decrypt failed; skipping revoke",
				"provider", provider, "user_id", userID, "error", err)
		} else if err := hooks.RevokeToken(ctx, refreshToken); err != nil {
			s.logger.Warn("streaming token revoke failed; continuing disconnect",
				"provider", provider, "user_id", userID, "error", err)
		}
	}
	return s.store.Delete(ctx, account.ID)
}

// CleanupForWithdrawal performs platform cleanup while retaining the database
// rows. The final account transaction removes those rows only after every
// external operation succeeds, so a failed request can safely retry.
func (s *StreamingAccountService) CleanupForWithdrawal(ctx context.Context, userID uuid.UUID) error {
	if err := s.ensureActive(ctx, userID); err != nil {
		return err
	}
	accounts, err := s.store.ListByUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		hooks := s.hooks[account.Provider]
		if hooks.CleanupResources != nil && account.StreamID != nil && *account.StreamID != "" {
			if err := hooks.CleanupResources(ctx, account); err != nil {
				if !errors.Is(err, ErrStreamingReconnectRequired) {
					return fmt.Errorf("cleanup %s resources: %w", account.Provider, err)
				}
				// A revoked refresh token cannot authenticate the provider's
				// delete call. Keep account deletion retryable by treating this
				// external state as permanently inaccessible and clearing the
				// server-side resource marker below. Token revocation remains
				// idempotent and is attempted with the same refresh token.
				if s.logger != nil {
					s.logger.Warn("streaming resource cleanup skipped because provider token is invalid",
						"provider", account.Provider, "user_id", userID, "stream_id", *account.StreamID)
				}
			}
			// Persist that no further provider cleanup is possible before
			// revoking the token. If a later withdrawal stage fails, the next
			// attempt can skip the already-deleted (or inaccessible) resource
			// and use the refresh token only for idempotent revocation.
			if account.StreamID != nil && *account.StreamID != "" {
				if err := s.store.UpdateStreamInfo(ctx, account.ID, StreamInfo{}); err != nil {
					return fmt.Errorf("clear %s stream resource metadata: %w", account.Provider, err)
				}
			}
		}
		if hooks.RevokeToken == nil || len(account.RefreshTokenCiphertext) == 0 {
			continue
		}
		if s.cipher == nil {
			return ErrWithdrawalUnavailable
		}
		refreshToken, err := s.cipher.Decrypt(account.RefreshTokenCiphertext, account.TokenKeyVersion)
		if err != nil {
			return fmt.Errorf("decrypt %s refresh token: %w", account.Provider, err)
		}
		if err := hooks.RevokeToken(ctx, refreshToken); err != nil {
			return fmt.Errorf("revoke %s refresh token: %w", account.Provider, err)
		}
	}
	return nil
}

// List는 사용자의 플랫폼 연결 목록을 돌려준다. 연결이 없으면 빈 슬라이스다
// (404가 아니라 빈 배열 — 이슈 #88 계약).
func (s *StreamingAccountService) List(ctx context.Context, userID uuid.UUID) ([]StreamingAccountSummary, error) {
	release, admitted := s.beginOperation(userID)
	if !admitted {
		return nil, ErrWithdrawalInProgress
	}
	defer release()

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

func (s *StreamingAccountService) beginOperation(userID uuid.UUID) (func(), bool) {
	if s == nil || s.gate == nil {
		return func() {}, true
	}
	return s.gate.BeginOperation(userID)
}
