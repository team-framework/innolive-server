package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	googleOAuthTokenEndpoint  = "https://oauth2.googleapis.com/token"
	googleOAuthRevokeEndpoint = "https://oauth2.googleapis.com/revoke"
	youtubeChannelsEndpoint   = "https://www.googleapis.com/youtube/v3/channels"
	// YouTubeStreamingScope는 Live API까지 포함하는 최소 스코프다. 이보다 좁은
	// 라이브 전용 스코프는 존재하지 않는다(2026-08-09 조사). 클라이언트 SDK가
	// serverAuthCode를 요청할 때 같은 값을 써야 한다.
	YouTubeStreamingScope = "https://www.googleapis.com/auth/youtube"
	maxYouTubeOAuthField  = 512
	// accessTokenExpirySlack만큼 만료를 앞당겨 취급해, 발급 직후 만료되는
	// 토큰으로 API를 치는 경계 상황을 피한다.
	accessTokenExpirySlack = 60 * time.Second
)

var (
	ErrYouTubeChannelMissing   = errors.New("Google account has no YouTube channel")
	ErrYouTubeTokenExchange    = errors.New("YouTube token exchange failed")
	ErrYouTubeAuthCodeRejected = errors.New("YouTube authorization code was rejected")
	ErrStreamingNotConnected   = errors.New("streaming account is not connected")
	// ErrStreamingReconnectRequired: 저장된 refresh token이 무효화됨(사용자의
	// 플랫폼 쪽 권한 취소 등). 재시도로 복구되지 않으며 재연결이 유일한 해법
	// 이라 일반 실패와 구분한다.
	ErrStreamingReconnectRequired = errors.New("streaming account requires reconnection")
)

// CodeSource는 인가 코드를 발급받은 클라이언트 유형이다. 교환 시 요구되는
// redirect_uri가 유형마다 달라(2026-08-10 실측: 웹 팝업 코드는 생략 시
// "Missing parameter: redirect_uri"로 거절, redirect_uri=postmessage 필수)
// 클라이언트가 출처를 선언하고 서버가 매핑한다. 출처를 잘못 선언해도 교환이
// 실패할 뿐 보안 영향은 없다(교환 자격증명은 서버의 웹 클라이언트 것 하나).
type CodeSource string

const (
	// CodeSourceNative: GoogleSignIn SDK의 serverAuthCode. 교환 시
	// redirect_uri 생략(문서 기준 — iOS 실물 코드로 최종 확인 예정).
	CodeSourceNative CodeSource = "native"
	// CodeSourceWebPopup: GIS initCodeClient(ux_mode: popup)가 발급한 코드.
	// 교환 시 redirect_uri=postmessage 필수(실측 확정).
	CodeSourceWebPopup CodeSource = "web_popup"
)

func (s CodeSource) Valid() bool {
	return s == CodeSourceNative || s == CodeSourceWebPopup
}

// exchangeRedirectURI는 출처별 교환 redirect_uri 값이다. 빈 문자열은
// 파라미터 생략을 뜻한다.
func (s CodeSource) exchangeRedirectURI() string {
	if s == CodeSourceWebPopup {
		return "postmessage"
	}
	return ""
}

// YouTubeOAuthConfig는 송출 연동 전용 Google OAuth 클라이언트 설정이다.
// 로그인용(GOOGLE_OAUTH_WEB_CLIENT_ID)과는 별도 GCP 프로젝트·클라이언트다.
type YouTubeOAuthConfig struct {
	ClientID     string
	ClientSecret string
}

func LoadYouTubeOAuthConfigFromEnv() (YouTubeOAuthConfig, error) {
	config := YouTubeOAuthConfig{
		ClientID:     strings.TrimSpace(os.Getenv("YOUTUBE_OAUTH_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(os.Getenv("YOUTUBE_OAUTH_CLIENT_SECRET")),
	}
	for _, value := range []string{config.ClientID, config.ClientSecret} {
		if utf8.RuneCountInString(value) > maxYouTubeOAuthField {
			return YouTubeOAuthConfig{}, errors.New("YouTube OAuth configuration value is too long")
		}
	}
	if config.ClientID == "" && config.ClientSecret == "" {
		return YouTubeOAuthConfig{}, nil
	}
	if config.ClientID == "" || config.ClientSecret == "" {
		return YouTubeOAuthConfig{}, errors.New("YOUTUBE_OAUTH_CLIENT_ID and YOUTUBE_OAUTH_CLIENT_SECRET must be configured together")
	}
	return config, nil
}

func (c YouTubeOAuthConfig) Enabled() bool { return c.ClientID != "" }

// YouTubeTokenResponse는 Google 토큰 엔드포인트 응답이다(2026-08-09 실측 기준).
type YouTubeTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	// RefreshTokenExpiresIn은 앱 게시 상태가 Testing일 때만 온다(실측 604799
	// = 7일). 0이면 refresh token은 무기한이다.
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	Scope                 string `json:"scope"`
	TokenType             string `json:"token_type"`
}

type YouTubeChannel struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// YouTubeAuthorizer는 Google OAuth·YouTube Data API와의 통신 계약이다.
// 서비스 계층 테스트에서 대역으로 대체된다.
type YouTubeAuthorizer interface {
	// Exchange의 redirectURI는 코드 출처별 요구값(CodeSource.exchangeRedirectURI)
	// 이며, 빈 문자열이면 파라미터를 생략한다.
	Exchange(ctx context.Context, code, redirectURI string) (YouTubeTokenResponse, error)
	RefreshAccessToken(ctx context.Context, refreshToken string) (YouTubeTokenResponse, error)
	ChannelForToken(ctx context.Context, accessToken string) (YouTubeChannel, error)
	// WebClientID는 웹 클라이언트(GIS)가 쓸 공개 클라이언트 식별자다.
	WebClientID() string
}

type youtubeOAuthClient struct {
	config      YouTubeOAuthConfig
	httpClient  *http.Client
	tokenURL    string
	revokeURL   string
	channelsURL string
}

func NewYouTubeOAuthClient(config YouTubeOAuthConfig) (*youtubeOAuthClient, error) {
	if !config.Enabled() {
		return nil, errors.New("YouTube OAuth is not configured")
	}
	return &youtubeOAuthClient{
		config:      config,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		tokenURL:    googleOAuthTokenEndpoint,
		revokeURL:   googleOAuthRevokeEndpoint,
		channelsURL: youtubeChannelsEndpoint,
	}, nil
}

// RevokeToken은 refresh token으로 부여된 권한 전체를 Google 쪽에서 취소한다
// (연결 해제 시 사용자의 Google 계정에 "InnoLive 권한 부여됨"이 남지 않게).
// 이미 무효한 토큰이면 Google이 400을 주는데, 해제 관점에선 목적이 달성된
// 상태이므로 에러로 다루지 않는다.
func (c *youtubeOAuthClient) RevokeToken(ctx context.Context, token string) error {
	form := url.Values{"token": {strings.TrimSpace(token)}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.revokeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("request Google token revocation: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
	if response.StatusCode == http.StatusOK || response.StatusCode == http.StatusBadRequest {
		return nil
	}
	return fmt.Errorf("Google token revocation returned HTTP %d", response.StatusCode)
}

// Exchange는 클라이언트가 획득한 인가 코드를 토큰으로 교환한다. 코드에 '/'
// 등 예약 문자가 들어오므로(실측) 반드시 form 인코딩을 거친다.
func (c *youtubeOAuthClient) Exchange(ctx context.Context, code, redirectURI string) (YouTubeTokenResponse, error) {
	form := url.Values{
		"client_id":     {c.config.ClientID},
		"client_secret": {c.config.ClientSecret},
		"code":          {strings.TrimSpace(code)},
		"grant_type":    {"authorization_code"},
	}
	if redirectURI != "" {
		form.Set("redirect_uri", redirectURI)
	}
	return c.requestToken(ctx, form)
}

func (c *youtubeOAuthClient) WebClientID() string { return c.config.ClientID }

// RefreshAccessToken은 refresh token으로 새 access token을 받는다. Google은
// refresh token을 회전하지 않는 것이 기본이지만, 응답에 새 값이 오면 호출자가
// 교체 저장해야 한다.
func (c *youtubeOAuthClient) RefreshAccessToken(ctx context.Context, refreshToken string) (YouTubeTokenResponse, error) {
	form := url.Values{
		"client_id":     {c.config.ClientID},
		"client_secret": {c.config.ClientSecret},
		"refresh_token": {strings.TrimSpace(refreshToken)},
		"grant_type":    {"refresh_token"},
	}
	return c.requestToken(ctx, form)
}

func (c *youtubeOAuthClient) requestToken(ctx context.Context, form url.Values) (YouTubeTokenResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return YouTubeTokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return YouTubeTokenResponse{}, fmt.Errorf("request Google token endpoint: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// 응답 본문을 버리면 실패 원인(client_secret 불일치·코드 만료·
		// redirect_uri 불일치)을 서버에서도 알 수 없다. 자격증명은 요청에만
		// 있고 응답에는 없으므로 error/error_description은 남겨도 안전하다.
		detail := googleTokenErrorDetail(response.Body)
		// 4xx는 코드 자체의 문제(만료·재사용·위조)라 클라이언트 오류로,
		// 그 외는 Google 쪽 장애로 구분한다.
		if response.StatusCode >= 400 && response.StatusCode < 500 {
			return YouTubeTokenResponse{}, fmt.Errorf("%w: HTTP %d%s", ErrYouTubeAuthCodeRejected, response.StatusCode, detail)
		}
		return YouTubeTokenResponse{}, fmt.Errorf("%w: HTTP %d%s", ErrYouTubeTokenExchange, response.StatusCode, detail)
	}
	var result YouTubeTokenResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 32<<10)).Decode(&result); err != nil {
		return YouTubeTokenResponse{}, fmt.Errorf("decode Google token response: %w", err)
	}
	if strings.TrimSpace(result.AccessToken) == "" {
		return YouTubeTokenResponse{}, fmt.Errorf("%w: response has no access_token", ErrYouTubeTokenExchange)
	}
	return result, nil
}

// googleTokenErrorDetail은 토큰 엔드포인트 오류 응답의 error /
// error_description을 사람이 읽을 수 있는 접미사로 만든다. 본문이 JSON이
// 아니거나 필드가 없으면 빈 문자열을 돌려준다.
func googleTokenErrorDetail(body io.Reader) string {
	var payload struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	limited := io.LimitReader(body, 8<<10)
	err := json.NewDecoder(limited).Decode(&payload)
	// 본문을 끝까지 읽어야 연결이 재사용된다 — 종전 io.Copy(io.Discard) 가
	// 하던 일이다.
	_, _ = io.Copy(io.Discard, limited)
	if err != nil {
		return ""
	}
	code := truncateTokenMetadata(strings.TrimSpace(payload.Error), 128)
	description := truncateTokenMetadata(strings.TrimSpace(payload.Description), 256)
	switch {
	case code != "" && description != "":
		return fmt.Sprintf(" (%s: %s)", code, description)
	case code != "":
		return " (" + code + ")"
	case description != "":
		return " (" + description + ")"
	}
	return ""
}

// ChannelForToken은 토큰이 매핑되는 YouTube 채널을 식별한다. 채널이 없는
// Google 계정(실사용 가능 시나리오)은 ErrYouTubeChannelMissing으로 구분한다.
func (c *youtubeOAuthClient) ChannelForToken(ctx context.Context, accessToken string) (YouTubeChannel, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.channelsURL+"?part=snippet&mine=true", nil)
	if err != nil {
		return YouTubeChannel{}, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return YouTubeChannel{}, fmt.Errorf("request YouTube channels: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
		return YouTubeChannel{}, fmt.Errorf("YouTube channels.list returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Items []struct {
			ID      string `json:"id"`
			Snippet struct {
				Title string `json:"title"`
			} `json:"snippet"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 256<<10)).Decode(&payload); err != nil {
		return YouTubeChannel{}, fmt.Errorf("decode YouTube channels response: %w", err)
	}
	if len(payload.Items) == 0 {
		return YouTubeChannel{}, ErrYouTubeChannelMissing
	}
	item := payload.Items[0]
	if strings.TrimSpace(item.ID) == "" {
		return YouTubeChannel{}, errors.New("YouTube channels response has no channel id")
	}
	return YouTubeChannel{ID: item.ID, Title: item.Snippet.Title}, nil
}

// YouTubeConnectService는 클라이언트(네이티브 SDK 또는 웹 GIS 팝업)가 획득한
// 인가 코드로 계정 연결을 완결한다. 브라우저 리다이렉트가 없으므로 state가
// 필요 없고, 사용자 바인딩은 connect 엔드포인트의 Bearer 인증이 담당한다.
type YouTubeConnectService struct {
	oauth  YouTubeAuthorizer
	store  StreamingAccountStore
	users  UserStatusChecker
	cipher *ProviderTokenCipher
	now    func() time.Time
}

func NewYouTubeConnectService(oauth YouTubeAuthorizer, store StreamingAccountStore, users UserStatusChecker, cipher *ProviderTokenCipher) (*YouTubeConnectService, error) {
	if oauth == nil || store == nil || users == nil || cipher == nil {
		return nil, errors.New("YouTube connect dependencies must not be nil")
	}
	return &YouTubeConnectService{
		oauth:  oauth,
		store:  store,
		users:  users,
		cipher: cipher,
		now:    func() time.Time { return time.Now().UTC() },
	}, nil
}

// ConnectWithAuthCode는 인가 코드를 토큰으로 교환하고 채널을 식별해
// 연결을 저장한다. connect 엔드포인트의 인라인 Bearer 인증은 사용자 상태를
// 확인하지 않으므로 여기서 active를 확인한다.
func (s *YouTubeConnectService) ConnectWithAuthCode(ctx context.Context, userID uuid.UUID, code string, source CodeSource) (YouTubeChannel, error) {
	if err := s.ensureActive(ctx, userID); err != nil {
		return YouTubeChannel{}, err
	}
	token, err := s.oauth.Exchange(ctx, code, source.exchangeRedirectURI())
	if err != nil {
		return YouTubeChannel{}, err
	}
	// refresh token 없이 연결을 저장하면 access token 만료(1시간) 후 송출이
	// 조용히 죽는다. prompt=consent로 항상 와야 하며, 안 왔다면 연결 실패다.
	if strings.TrimSpace(token.RefreshToken) == "" {
		return YouTubeChannel{}, fmt.Errorf("%w: response has no refresh_token", ErrYouTubeTokenExchange)
	}
	channel, err := s.oauth.ChannelForToken(ctx, token.AccessToken)
	if err != nil {
		return YouTubeChannel{}, err
	}
	ciphertext, version, err := s.cipher.Encrypt(token.RefreshToken)
	if err != nil {
		return YouTubeChannel{}, err
	}
	account := StreamingAccount{
		UserID:                 userID,
		Provider:               StreamingProviderYouTube,
		ChannelID:              channel.ID,
		ChannelTitle:           googleOptionalString(channel.Title, 255),
		RefreshTokenCiphertext: ciphertext,
		TokenKeyVersion:        version,
		RefreshTokenExpiresAt:  s.refreshTokenExpiry(token),
	}
	if err := s.store.Upsert(ctx, account); err != nil {
		return YouTubeChannel{}, fmt.Errorf("persist streaming account: %w", err)
	}
	return channel, nil
}

// WebClientID는 웹(GIS) 클라이언트가 initCodeClient에 쓸 공개 클라이언트
// 식별자다 — 비밀이 아니며 GET /auth/youtube/config 로 노출된다.
func (s *YouTubeConnectService) WebClientID() string {
	return s.oauth.WebClientID()
}

func (s *YouTubeConnectService) ensureActive(ctx context.Context, userID uuid.UUID) error {
	status, err := s.users.UserStatus(ctx, userID)
	if err != nil {
		return err
	}
	if status != UserStatusActive {
		return ErrUserInactive
	}
	return nil
}

func (s *YouTubeConnectService) refreshTokenExpiry(token YouTubeTokenResponse) *time.Time {
	if token.RefreshTokenExpiresIn <= 0 {
		return nil
	}
	expiresAt := s.now().Add(time.Duration(token.RefreshTokenExpiresIn) * time.Second)
	return &expiresAt
}

// YouTubeAccessTokenProvider는 저장된 refresh token으로 access token을
// 발급·캐시한다. 갱신은 사용자 단위로 직렬화한다 — 한 사용자의 방송 세션
// 여러 개가 동시에 만료를 만나도 토큰 엔드포인트 호출은 한 번만 나간다.
type YouTubeAccessTokenProvider struct {
	oauth  YouTubeAuthorizer
	store  StreamingAccountStore
	cipher *ProviderTokenCipher
	now    func() time.Time

	mu    sync.Mutex
	users map[uuid.UUID]*userAccessToken
}

type userAccessToken struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func NewYouTubeAccessTokenProvider(oauth YouTubeAuthorizer, store StreamingAccountStore, cipher *ProviderTokenCipher) (*YouTubeAccessTokenProvider, error) {
	if oauth == nil || store == nil || cipher == nil {
		return nil, errors.New("YouTube token provider dependencies must not be nil")
	}
	return &YouTubeAccessTokenProvider{
		oauth:  oauth,
		store:  store,
		cipher: cipher,
		now:    func() time.Time { return time.Now().UTC() },
		users:  make(map[uuid.UUID]*userAccessToken),
	}, nil
}

// AccessToken은 유효한 access token을 돌려준다. 캐시가 만료 여유(60초) 안에
// 있으면 그대로 쓰고, 아니면 refresh token으로 갱신한다.
func (p *YouTubeAccessTokenProvider) AccessToken(ctx context.Context, userID uuid.UUID) (string, error) {
	state := p.userState(userID)
	// 사용자 단위 락: 같은 사용자의 동시 호출은 첫 갱신을 기다렸다가 캐시를
	// 재사용한다. 다른 사용자끼리는 서로 막지 않는다.
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.token != "" && p.now().Before(state.expiresAt.Add(-accessTokenExpirySlack)) {
		return state.token, nil
	}
	account, err := p.store.Get(ctx, userID, StreamingProviderYouTube)
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
		// 토큰 엔드포인트의 4xx는 refresh token 자체가 무효라는 뜻이다(만료·
		// 권한 취소). 재연결 필요로 표식하고 전용 에러로 구분한다 — 표식
		// 실패는 무시한다(다음 시도에서 다시 표식된다).
		if errors.Is(err, ErrYouTubeAuthCodeRejected) {
			_ = p.store.MarkReconnectRequired(ctx, account.ID, p.now())
			return "", fmt.Errorf("%w: %v", ErrStreamingReconnectRequired, err)
		}
		return "", err
	}
	// Google이 예외적으로 새 refresh token을 주면 행 락 하에 교체 저장한다.
	if next := strings.TrimSpace(response.RefreshToken); next != "" && next != refreshToken {
		ciphertext, version, err := p.cipher.Encrypt(next)
		if err != nil {
			return "", err
		}
		var expiresAt *time.Time
		if response.RefreshTokenExpiresIn > 0 {
			at := p.now().Add(time.Duration(response.RefreshTokenExpiresIn) * time.Second)
			expiresAt = &at
		}
		if err := p.store.UpdateRefreshToken(ctx, account.ID, ciphertext, version, expiresAt); err != nil {
			return "", fmt.Errorf("persist rotated refresh token: %w", err)
		}
	}
	state.token = response.AccessToken
	state.expiresAt = p.now().Add(time.Duration(response.ExpiresIn) * time.Second)
	return state.token, nil
}

func (p *YouTubeAccessTokenProvider) userState(userID uuid.UUID) *userAccessToken {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.users[userID]
	if state == nil {
		state = &userAccessToken{}
		p.users[userID] = state
	}
	return state
}
