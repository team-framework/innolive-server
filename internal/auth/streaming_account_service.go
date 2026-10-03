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
	// ID는 연결 ID다. 유튜브 채널이 여러 개면 방송 준비·해제에 이 값을 쓴다(#390).
	ID           uuid.UUID         `json:"id"`
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
	// ClearTokenCache는 서버 메모리에 캐시한 access token을 지운다. 지우지 않으면
	// 해제한 계정의 토큰을 만료 전까지 계속 쓴다(#340).
	ClearTokenCache func(userID uuid.UUID)
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
	inUse func(userID uuid.UUID, provider StreamingProvider, accountID uuid.UUID) bool
}

// ErrStreamingAccountInUse는 그 플랫폼으로 방송을 준비·송출하는 중이라 연결을
// 해제할 수 없다는 뜻이다(#348). 해제하면 토큰이 사라져 영상은 계속 나가는데
// 플랫폼 방송은 정리하지 못한다.
var ErrStreamingAccountInUse = errors.New("streaming account is in use by an active broadcast")

// SetInUseChecker는 연결이 방송에 쓰이는 중인지 알려 줄 함수를 붙인다. auth는
// 세션을 모르므로 조립 단계에서 주입한다. 유튜브는 채널이 여러 개일 수 있어 연결
// ID까지 넘긴다 — 방송에 고정된 채널만 해제를 막는다(#390).
func (s *StreamingAccountService) SetInUseChecker(inUse func(userID uuid.UUID, provider StreamingProvider, accountID uuid.UUID) bool) {
	if s != nil {
		s.inUse = inUse
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

// SetUserOperationGate는 새 연결·해제 요청이 계정 탈퇴와 경쟁하지 않게 한다. 탈퇴
// 정리 메서드는 이미 사용자의 독점 자리를 쥐고 있어 이 게이트를 일부러 건너뛴다.
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
//
// accountID가 nil이면 그 플랫폼 연결이 하나일 때만 해제한다. 유튜브 채널이 여러
// 개인데 지정이 없으면 ErrStreamingAccountSelectionRequired.
func (s *StreamingAccountService) Disconnect(ctx context.Context, userID uuid.UUID, provider StreamingProvider, accountID *uuid.UUID) error {
	release, admitted := s.beginOperation(userID)
	if !admitted {
		return ErrWithdrawalInProgress
	}
	defer release()

	if err := s.ensureActive(ctx, userID); err != nil {
		return err
	}
	if accountID != nil {
		ctx = WithStreamingAccount(ctx, *accountID)
	}
	account, err := s.store.Get(ctx, userID, provider)
	if err != nil {
		return err
	}
	if s.inUse != nil && s.inUse(userID, provider, account.ID) {
		return ErrStreamingAccountInUse
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
	err = s.store.Delete(ctx, account.ID)
	// 행을 지운 뒤에 캐시를 비운다 — 그 사이 토큰을 새로 받으려 해도 행이 없다.
	if hooks.ClearTokenCache != nil {
		hooks.ClearTokenCache(userID)
	}
	return err
}

// CleanupForWithdrawal은 DB 행을 남긴 채 플랫폼 정리를 한다. 마지막 계정
// 트랜잭션이 모든 외부 작업이 성공한 뒤에만 그 행을 지우므로, 실패한 요청을
// 안전하게 다시 시도할 수 있다.
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
				// 취소된 refresh token으로는 플랫폼 삭제 호출을 인증할 수 없다. 이 외부
				// 상태를 영구히 접근 불가로 보고 아래에서 서버 쪽 리소스 표시를 지워 계정
				// 삭제를 재시도 가능하게 둔다. 토큰 취소는 멱등이라 같은 refresh token으로
				// 시도한다.
				if s.logger != nil {
					s.logger.Warn("streaming resource cleanup skipped because provider token is invalid",
						"provider", account.Provider, "user_id", userID, "stream_id", *account.StreamID)
				}
			}
			// 토큰을 취소하기 전에 더는 플랫폼 정리를 할 수 없다는 사실을 저장한다. 이후
			// 탈퇴 단계가 실패해도 다음 시도는 이미 지워진(또는 접근할 수 없는) 리소스를
			// 건너뛰고 refresh token은 멱등인 취소에만 쓴다.
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
			ID:                account.ID,
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
