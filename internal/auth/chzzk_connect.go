package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// chzzkRefreshTokenTTL은 치지직 refresh token의 수명이다(실측 30일). 토큰
// 응답이 refresh 만료를 따로 주지 않으므로 발급 시각에 더해 기록한다 —
// 이 값이 있어야 ReconnectRequired 판정이 API 호출 없이 성립한다.
const chzzkRefreshTokenTTL = 30 * 24 * time.Hour

// ChzzkConnectService는 인가 코드를 토큰으로 교환하고 채널을 식별해 연결을
// 저장한다. 유튜브 경로와 같은 구조이며, 다른 점은 스코프 검사와 refresh
// 만료를 응답이 아니라 수명 상수로 계산한다는 것뿐이다.
type ChzzkConnectService struct {
	oauth  ChzzkAuthorizer
	store  StreamingAccountStore
	users  UserStatusChecker
	cipher *ProviderTokenCipher
	now    func() time.Time
	gate   interface {
		BeginOperation(uuid.UUID) (func(), bool)
	}
	clearTokenCache func(uuid.UUID)
}

func NewChzzkConnectService(oauth ChzzkAuthorizer, store StreamingAccountStore, users UserStatusChecker, cipher *ProviderTokenCipher) (*ChzzkConnectService, error) {
	if oauth == nil || store == nil || users == nil || cipher == nil {
		return nil, errors.New("Chzzk connect service dependencies must not be nil")
	}
	return &ChzzkConnectService{
		oauth:  oauth,
		store:  store,
		users:  users,
		cipher: cipher,
		now:    func() time.Time { return time.Now().UTC() },
	}, nil
}

func (s *ChzzkConnectService) SetUserOperationGate(gate interface {
	BeginOperation(uuid.UUID) (func(), bool)
}) {
	s.gate = gate
}

func (s *ChzzkConnectService) SetTokenCacheInvalidator(clear func(uuid.UUID)) {
	s.clearTokenCache = clear
}

// ClientID / RedirectURI / AuthorizeURL은 전부 공개 값이라 config 엔드포인트로
// 그대로 나간다. Secret은 어떤 경로로도 노출되지 않는다.
func (s *ChzzkConnectService) ClientID() string                 { return s.oauth.ClientID() }
func (s *ChzzkConnectService) RedirectURI() string              { return s.oauth.RedirectURI() }
func (s *ChzzkConnectService) AuthorizeURL(state string) string { return s.oauth.AuthorizeURL(state) }

// ConnectWithAuthCode는 콜백 페이지가 릴레이한 code를 연결로 완결한다.
// state는 클라이언트가 만들고 콜백에서 대조하며, 교환 요청에 그대로 실어
// 보낸다 — 서버는 state를 보관하지 않는다.
func (s *ChzzkConnectService) ConnectWithAuthCode(ctx context.Context, userID uuid.UUID, code, state string) (ChzzkChannel, error) {
	release, admitted := s.beginOperation(userID)
	if !admitted {
		return ChzzkChannel{}, ErrWithdrawalInProgress
	}
	defer release()

	if err := s.ensureActive(ctx, userID); err != nil {
		return ChzzkChannel{}, err
	}
	token, err := s.oauth.Exchange(ctx, code, state)
	if err != nil {
		return ChzzkChannel{}, err
	}
	// 사용자가 동의 화면에서 권한 일부를 빼면 연결은 되지만 송출이 실패한다.
	// 저장하기 전에 거절해, 실패를 방송 직전이 아니라 연결 시점으로 옮긴다.
	if ok, missing := ChzzkHasScopes(token.Scope, ChzzkRequiredScopes); !ok {
		return ChzzkChannel{}, fmt.Errorf("%w: %s", ErrChzzkScopeMissing, strings.Join(missing, ", "))
	}
	channel, err := s.oauth.ChannelForToken(ctx, token.AccessToken)
	if err != nil {
		return ChzzkChannel{}, err
	}
	ciphertext, version, err := s.cipher.Encrypt(token.RefreshToken)
	if err != nil {
		return ChzzkChannel{}, err
	}
	expiresAt := s.now().Add(chzzkRefreshTokenTTL)
	account := StreamingAccount{
		UserID:                 userID,
		Provider:               StreamingProviderChzzk,
		ChannelID:              channel.ID,
		ChannelTitle:           chzzkOptionalString(channel.Name, 255),
		RefreshTokenCiphertext: ciphertext,
		TokenKeyVersion:        version,
		RefreshTokenExpiresAt:  &expiresAt,
	}
	if err := s.store.Upsert(ctx, account); err != nil {
		return ChzzkChannel{}, fmt.Errorf("persist streaming account: %w", err)
	}
	if s.clearTokenCache != nil {
		s.clearTokenCache(userID)
	}
	return channel, nil
}

func (s *ChzzkConnectService) beginOperation(userID uuid.UUID) (func(), bool) {
	if s == nil || s.gate == nil {
		return func() {}, true
	}
	return s.gate.BeginOperation(userID)
}

func (s *ChzzkConnectService) ensureActive(ctx context.Context, userID uuid.UUID) error {
	status, err := s.users.UserStatus(ctx, userID)
	if err != nil {
		return err
	}
	if status != UserStatusActive {
		return ErrUserInactive
	}
	return nil
}

func chzzkOptionalString(value string, limit int) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	if len([]rune(trimmed)) > limit {
		trimmed = string([]rune(trimmed)[:limit])
	}
	return &trimmed
}

// ChzzkAccessTokenProvider는 저장된 refresh token으로 access token을 발급·
// 캐시한다. 치지직 refresh는 일회용이라 갱신할 때마다 새 값을 반드시
// 저장해야 한다 — 저장에 실패하면 그 토큰은 영영 못 쓴다.
type ChzzkAccessTokenProvider struct {
	oauth  ChzzkAuthorizer
	store  StreamingAccountStore
	cipher *ProviderTokenCipher
	now    func() time.Time

	mu    sync.Mutex
	users map[uuid.UUID]*userAccessToken
}

func NewChzzkAccessTokenProvider(oauth ChzzkAuthorizer, store StreamingAccountStore, cipher *ProviderTokenCipher) (*ChzzkAccessTokenProvider, error) {
	if oauth == nil || store == nil || cipher == nil {
		return nil, errors.New("Chzzk token provider dependencies must not be nil")
	}
	return &ChzzkAccessTokenProvider{
		oauth:  oauth,
		store:  store,
		cipher: cipher,
		now:    func() time.Time { return time.Now().UTC() },
		users:  make(map[uuid.UUID]*userAccessToken),
	}, nil
}

func (p *ChzzkAccessTokenProvider) AccessToken(ctx context.Context, userID uuid.UUID) (string, error) {
	state := p.userState(userID)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.token != "" && p.now().Before(state.expiresAt.Add(-accessTokenExpirySlack)) {
		return state.token, nil
	}
	account, err := p.store.Get(ctx, userID, StreamingProviderChzzk)
	if err != nil {
		if errors.Is(err, ErrStreamingAccountNotFound) {
			return "", ErrStreamingNotConnected
		}
		return "", err
	}
	if len(account.RefreshTokenCiphertext) == 0 {
		return "", ErrStreamingNotConnected
	}
	refreshToken, err := p.cipher.Decrypt(account.RefreshTokenCiphertext, account.TokenKeyVersion)
	if err != nil {
		return "", err
	}
	response, err := p.oauth.RefreshAccessToken(ctx, refreshToken)
	if err != nil {
		if errors.Is(err, ErrChzzkAuthCodeRejected) {
			_ = p.store.MarkReconnectRequired(ctx, account.ID, p.now())
			return "", fmt.Errorf("%w: %v", ErrStreamingReconnectRequired, err)
		}
		return "", err
	}
	// 회전한 refresh를 먼저 저장하고 나서 access token을 캐시한다. 순서가
	// 반대면 저장 실패 시 못 쓰는 토큰을 든 채로 성공을 돌려주게 된다.
	if next := strings.TrimSpace(response.RefreshToken); next != "" && next != refreshToken {
		ciphertext, version, err := p.cipher.Encrypt(next)
		if err != nil {
			return "", err
		}
		expiresAt := p.now().Add(chzzkRefreshTokenTTL)
		if err := p.store.UpdateRefreshToken(ctx, account.ID, ciphertext, version, &expiresAt); err != nil {
			return "", fmt.Errorf("persist rotated refresh token: %w", err)
		}
	}
	state.token = response.AccessToken
	state.expiresAt = p.now().Add(time.Duration(response.ExpiresIn) * time.Second)
	return state.token, nil
}

func (p *ChzzkAccessTokenProvider) ClearCachedToken(userID uuid.UUID) {
	if p == nil || userID == uuid.Nil {
		return
	}
	p.mu.Lock()
	delete(p.users, userID)
	p.mu.Unlock()
}

func (p *ChzzkAccessTokenProvider) userState(userID uuid.UUID) *userAccessToken {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.users[userID]
	if state == nil {
		state = &userAccessToken{}
		p.users[userID] = state
	}
	return state
}
