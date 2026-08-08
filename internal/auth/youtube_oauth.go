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
	googleOAuthAuthorizeEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"
	googleOAuthTokenEndpoint     = "https://oauth2.googleapis.com/token"
	youtubeChannelsEndpoint      = "https://www.googleapis.com/youtube/v3/channels"
	// youtubeStreamingScope는 Live API까지 포함하는 최소 스코프다. 이보다 좁은
	// 라이브 전용 스코프는 존재하지 않는다(2026-08-09 조사).
	youtubeStreamingScope = "https://www.googleapis.com/auth/youtube"
	maxYouTubeOAuthField  = 512
	oauthStateTTL         = 10 * time.Minute
	// accessTokenExpirySlack만큼 만료를 앞당겨 취급해, 발급 직후 만료되는
	// 토큰으로 API를 치는 경계 상황을 피한다.
	accessTokenExpirySlack = 60 * time.Second
)

var (
	ErrYouTubeOAuthDenied     = errors.New("YouTube authorization was denied by the user")
	ErrInvalidOAuthState      = errors.New("OAuth state is invalid or expired")
	ErrYouTubeChannelMissing  = errors.New("Google account has no YouTube channel")
	ErrYouTubeTokenExchange   = errors.New("YouTube token exchange failed")
	ErrStreamingNotConnected  = errors.New("streaming account is not connected")
)

// YouTubeOAuthConfig는 송출 연동 전용 Google OAuth 클라이언트 설정이다.
// 로그인용(GOOGLE_OAUTH_WEB_CLIENT_ID)과는 별도 GCP 프로젝트·클라이언트다.
type YouTubeOAuthConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

func LoadYouTubeOAuthConfigFromEnv() (YouTubeOAuthConfig, error) {
	config := YouTubeOAuthConfig{
		ClientID:     strings.TrimSpace(os.Getenv("YOUTUBE_OAUTH_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(os.Getenv("YOUTUBE_OAUTH_CLIENT_SECRET")),
		RedirectURI:  strings.TrimSpace(os.Getenv("YOUTUBE_OAUTH_REDIRECT_URI")),
	}
	values := []string{config.ClientID, config.ClientSecret, config.RedirectURI}
	empty := 0
	for _, value := range values {
		if value == "" {
			empty++
		}
		if utf8.RuneCountInString(value) > maxYouTubeOAuthField {
			return YouTubeOAuthConfig{}, errors.New("YouTube OAuth configuration value is too long")
		}
	}
	if empty == len(values) {
		return YouTubeOAuthConfig{}, nil
	}
	if empty != 0 {
		return YouTubeOAuthConfig{}, errors.New("YOUTUBE_OAUTH_CLIENT_ID, YOUTUBE_OAUTH_CLIENT_SECRET, and YOUTUBE_OAUTH_REDIRECT_URI must be configured together")
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
	AuthorizeURL(state string) string
	Exchange(ctx context.Context, code string) (YouTubeTokenResponse, error)
	RefreshAccessToken(ctx context.Context, refreshToken string) (YouTubeTokenResponse, error)
	ChannelForToken(ctx context.Context, accessToken string) (YouTubeChannel, error)
}

type youtubeOAuthClient struct {
	config       YouTubeOAuthConfig
	httpClient   *http.Client
	authorizeURL string
	tokenURL     string
	channelsURL  string
}

func NewYouTubeOAuthClient(config YouTubeOAuthConfig) (*youtubeOAuthClient, error) {
	if !config.Enabled() {
		return nil, errors.New("YouTube OAuth is not configured")
	}
	return &youtubeOAuthClient{
		config:       config,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		authorizeURL: googleOAuthAuthorizeEndpoint,
		tokenURL:     googleOAuthTokenEndpoint,
		channelsURL:  youtubeChannelsEndpoint,
	}, nil
}

// AuthorizeURL은 사용자를 보낼 Google 동의 화면 URL을 만든다.
// access_type=offline만으로는 재승인 시 refresh_token이 응답에서 빠질 수
// 있어 prompt=consent를 함께 강제한다(2026-08-09 실측으로 수신 확인).
func (c *youtubeOAuthClient) AuthorizeURL(state string) string {
	query := url.Values{
		"client_id":     {c.config.ClientID},
		"redirect_uri":  {c.config.RedirectURI},
		"response_type": {"code"},
		"scope":         {youtubeStreamingScope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	return c.authorizeURL + "?" + query.Encode()
}

// Exchange는 인가 코드를 토큰으로 교환한다. 코드에 '/' 등 예약 문자가
// 들어오므로(실측) 반드시 form 인코딩을 거친다.
func (c *youtubeOAuthClient) Exchange(ctx context.Context, code string) (YouTubeTokenResponse, error) {
	form := url.Values{
		"client_id":     {c.config.ClientID},
		"client_secret": {c.config.ClientSecret},
		"code":          {strings.TrimSpace(code)},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {c.config.RedirectURI},
	}
	return c.requestToken(ctx, form)
}

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
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
		return YouTubeTokenResponse{}, fmt.Errorf("%w: HTTP %d", ErrYouTubeTokenExchange, response.StatusCode)
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

// YouTubeConnectService는 계정 연결 플로우(connect → 동의 → callback)를
// 오케스트레이션한다.
type YouTubeConnectService struct {
	oauth  YouTubeAuthorizer
	states *oauthStateStore
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
		states: newOAuthStateStore(oauthStateTTL),
		store:  store,
		users:  users,
		cipher: cipher,
		now:    func() time.Time { return time.Now().UTC() },
	}, nil
}

// Connect는 연결 플로우를 시작한다: 사용자에 바인딩된 state를 발급하고
// 클라이언트가 이동할 authorize URL을 돌려준다. connect 엔드포인트의 인라인
// Bearer 인증은 사용자 상태를 확인하지 않으므로 여기서 active를 확인한다.
func (s *YouTubeConnectService) Connect(ctx context.Context, userID uuid.UUID) (string, error) {
	if err := s.ensureActive(ctx, userID); err != nil {
		return "", err
	}
	state, err := s.states.Issue(userID)
	if err != nil {
		return "", err
	}
	return s.oauth.AuthorizeURL(state), nil
}

// CompleteCallback은 콜백의 state로 사용자를 복원하고, 코드를 토큰으로 교환한
// 뒤 채널을 식별해 연결을 저장한다.
func (s *YouTubeConnectService) CompleteCallback(ctx context.Context, state, code string) (YouTubeChannel, error) {
	userID, ok := s.states.Consume(state)
	if !ok {
		return YouTubeChannel{}, ErrInvalidOAuthState
	}
	if err := s.ensureActive(ctx, userID); err != nil {
		return YouTubeChannel{}, err
	}
	token, err := s.oauth.Exchange(ctx, code)
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
