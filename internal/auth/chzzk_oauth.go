package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	chzzkAuthorizeEndpoint = "https://chzzk.naver.com/account-interlock"
	chzzkTokenEndpoint     = "https://openapi.chzzk.naver.com/auth/v1/token"
	chzzkRevokeEndpoint    = "https://openapi.chzzk.naver.com/auth/v1/token/revoke"
	chzzkUsersMeEndpoint   = "https://openapi.chzzk.naver.com/open/v1/users/me"
	maxChzzkOAuthField     = 512
	// chzzkResponseLimit은 플랫폼 응답을 읽을 때의 상한이다. 정상 응답은
	// 수백 바이트이므로 넉넉하면서도 무한정 읽지 않는다.
	chzzkResponseLimit = 64 << 10
)

// 치지직 스코프 표시명. 응답의 scope는 한글 표시명을 공백으로 이은 문자열인데
// 표시명 안에도 공백이 있어(예: "방송 설정 조회") split으로는 가를 수 없다.
// 따라서 이 이름들을 포함 검사로 판정한다.
const (
	ChzzkScopeUserRead         = "유저 조회"
	ChzzkScopeStreamKeyRead    = "방송 스트림키 조회"
	ChzzkScopeLiveSettingRead  = "방송 설정 조회"
	ChzzkScopeLiveSettingWrite = "방송 설정 변경"
)

// ChzzkRequiredScopes는 송출에 필요한 스코프 전체다.
var ChzzkRequiredScopes = []string{
	ChzzkScopeUserRead,
	ChzzkScopeStreamKeyRead,
	ChzzkScopeLiveSettingRead,
	ChzzkScopeLiveSettingWrite,
}

var (
	ErrChzzkTokenExchange    = errors.New("Chzzk token exchange failed")
	ErrChzzkAuthCodeRejected = errors.New("Chzzk authorization code was rejected")
	ErrChzzkChannelMissing   = errors.New("Chzzk account has no channel")
	// ErrChzzkScopeMissing: 사용자가 동의 화면에서 일부 권한을 뺐다. 연결을
	// 저장해도 송출이 실패하므로 연결 단계에서 거절한다.
	ErrChzzkScopeMissing = errors.New("Chzzk authorization is missing required scopes")
)

// ChzzkHasScopes는 토큰 응답의 scope 문자열이 필요한 스코프를 모두 담고
// 있는지 본다. 공백 split은 표시명을 쪼개므로 쓰지 않는다 — 부분 문자열
// 포함으로 판정한다. 없는 스코프 이름을 함께 돌려줘 호출자가 로그로 남긴다.
func ChzzkHasScopes(scope string, required []string) (bool, []string) {
	missing := []string{}
	for _, name := range required {
		if !strings.Contains(scope, name) {
			missing = append(missing, name)
		}
	}
	return len(missing) == 0, missing
}

type ChzzkOAuthConfig struct {
	ClientID     string
	ClientSecret string
	// RedirectURI는 치지직 개발자센터에 등록한 값과 문자 단위로 같아야 한다.
	// 개발·프로덕션이 다르므로 하드코딩하지 않는다.
	RedirectURI string
}

func LoadChzzkOAuthConfigFromEnv() (ChzzkOAuthConfig, error) {
	config := ChzzkOAuthConfig{
		ClientID:     strings.TrimSpace(os.Getenv("CHZZK_OAUTH_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(os.Getenv("CHZZK_OAUTH_CLIENT_SECRET")),
		RedirectURI:  strings.TrimSpace(os.Getenv("CHZZK_OAUTH_REDIRECT_URI")),
	}
	for _, value := range []string{config.ClientID, config.ClientSecret, config.RedirectURI} {
		if utf8.RuneCountInString(value) > maxChzzkOAuthField {
			return ChzzkOAuthConfig{}, errors.New("Chzzk OAuth configuration value is too long")
		}
	}
	if config.ClientID == "" && config.ClientSecret == "" && config.RedirectURI == "" {
		return ChzzkOAuthConfig{}, nil
	}
	if config.ClientID == "" || config.ClientSecret == "" || config.RedirectURI == "" {
		return ChzzkOAuthConfig{}, errors.New("CHZZK_OAUTH_CLIENT_ID, CHZZK_OAUTH_CLIENT_SECRET and CHZZK_OAUTH_REDIRECT_URI must be configured together")
	}
	return config, nil
}

func (c ChzzkOAuthConfig) Enabled() bool { return c.ClientID != "" }

// ChzzkTokenResponse는 토큰 엔드포인트 content의 필드다(2026-09-16 실측).
// access 1일 / refresh 30일이며 refresh는 일회용이라 갱신 응답의 값으로
// 반드시 교체해야 한다.
type ChzzkTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	TokenType    string `json:"tokenType"`
	ExpiresIn    int64  `json:"expiresIn"`
	Scope        string `json:"scope"`
}

// ChzzkChannel은 users/me 응답이다. nickname은 문서에 없지만 실제로 온다.
type ChzzkChannel struct {
	ID   string `json:"channelId"`
	Name string `json:"channelName"`
	Nick string `json:"nickname"`
}

// chzzkEnvelope는 치지직 공통 응답 봉투다. 성공은 code=200이고 content가
// 채워지며, 실패는 code/message만 온다.
type chzzkEnvelope struct {
	Code    int             `json:"code"`
	Message *string         `json:"message"`
	Content json.RawMessage `json:"content"`
}

type ChzzkAuthorizer interface {
	Exchange(ctx context.Context, code, state string) (ChzzkTokenResponse, error)
	RefreshAccessToken(ctx context.Context, refreshToken string) (ChzzkTokenResponse, error)
	ChannelForToken(ctx context.Context, accessToken string) (ChzzkChannel, error)
	RevokeToken(ctx context.Context, refreshToken string) error
	// AuthorizeURL은 사용자를 보낼 인가 페이지 주소다. state는 호출자가
	// 만들고 콜백에서 대조한다.
	AuthorizeURL(state string) string
	ClientID() string
	RedirectURI() string
}

type chzzkOAuthClient struct {
	config       ChzzkOAuthConfig
	httpClient   *http.Client
	authorizeURL string
	tokenURL     string
	revokeURL    string
	usersMeURL   string
}

func NewChzzkOAuthClient(config ChzzkOAuthConfig) (*chzzkOAuthClient, error) {
	if !config.Enabled() {
		return nil, errors.New("Chzzk OAuth is not configured")
	}
	return &chzzkOAuthClient{
		config:       config,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		authorizeURL: chzzkAuthorizeEndpoint,
		tokenURL:     chzzkTokenEndpoint,
		revokeURL:    chzzkRevokeEndpoint,
		usersMeURL:   chzzkUsersMeEndpoint,
	}, nil
}

func (c *chzzkOAuthClient) ClientID() string    { return c.config.ClientID }
func (c *chzzkOAuthClient) RedirectURI() string { return c.config.RedirectURI }

// AuthorizeURL은 clientId·redirectUri·state를 실은 인가 주소다. 셋 다 공개
// 값이라 GET /auth/chzzk/config로 그대로 내보낼 수 있다.
func (c *chzzkOAuthClient) AuthorizeURL(state string) string {
	query := url.Values{
		"clientId":    {c.config.ClientID},
		"redirectUri": {c.config.RedirectURI},
		"state":       {state},
	}
	return c.authorizeURL + "?" + query.Encode()
}

func (c *chzzkOAuthClient) Exchange(ctx context.Context, code, state string) (ChzzkTokenResponse, error) {
	return c.requestToken(ctx, map[string]string{
		"grantType":    "authorization_code",
		"clientId":     c.config.ClientID,
		"clientSecret": c.config.ClientSecret,
		"code":         code,
		"state":        state,
	})
}

func (c *chzzkOAuthClient) RefreshAccessToken(ctx context.Context, refreshToken string) (ChzzkTokenResponse, error) {
	return c.requestToken(ctx, map[string]string{
		"grantType":    "refresh_token",
		"clientId":     c.config.ClientID,
		"clientSecret": c.config.ClientSecret,
		"refreshToken": refreshToken,
	})
}

// RevokeToken은 연결 해제 시 치지직 쪽 권한 부여를 취소한다. 이미 무효한
// 토큰이면 플랫폼이 4xx를 주는데, 해제 관점에선 목적이 달성된 상태이므로
// 에러로 올리지 않는다(유튜브 경로와 같은 판단).
func (c *chzzkOAuthClient) RevokeToken(ctx context.Context, refreshToken string) error {
	payload := map[string]string{
		"clientId":      c.config.ClientID,
		"clientSecret":  c.config.ClientSecret,
		"token":         strings.TrimSpace(refreshToken),
		"tokenTypeHint": "refresh_token",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.revokeURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("request Chzzk token revocation: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, chzzkResponseLimit))
	if response.StatusCode < http.StatusInternalServerError {
		return nil
	}
	return fmt.Errorf("Chzzk token revocation returned HTTP %d", response.StatusCode)
}

func (c *chzzkOAuthClient) requestToken(ctx context.Context, payload map[string]string) (ChzzkTokenResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return ChzzkTokenResponse{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, bytes.NewReader(body))
	if err != nil {
		return ChzzkTokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return ChzzkTokenResponse{}, fmt.Errorf("%w: %v", ErrChzzkTokenExchange, err)
	}
	defer response.Body.Close()
	content, code, message, err := decodeChzzkEnvelope(response)
	if err != nil {
		return ChzzkTokenResponse{}, err
	}
	if code != http.StatusOK {
		// 코드·리프레시 거절은 재시도로 풀리지 않고 재연결이 유일한 해법이라
		// 통신 실패와 구분한다. message에는 자격증명이 실리지 않는다.
		if code == http.StatusBadRequest || code == http.StatusUnauthorized {
			return ChzzkTokenResponse{}, fmt.Errorf("%w: %s", ErrChzzkAuthCodeRejected, message)
		}
		return ChzzkTokenResponse{}, fmt.Errorf("%w: platform code %d: %s", ErrChzzkTokenExchange, code, message)
	}
	token := ChzzkTokenResponse{}
	if err := json.Unmarshal(content, &token); err != nil {
		return ChzzkTokenResponse{}, fmt.Errorf("%w: decode content: %v", ErrChzzkTokenExchange, err)
	}
	if strings.TrimSpace(token.AccessToken) == "" || strings.TrimSpace(token.RefreshToken) == "" {
		// refresh가 없으면 하루 뒤 송출이 조용히 죽는다. 연결로 치지 않는다.
		return ChzzkTokenResponse{}, fmt.Errorf("%w: response is missing tokens", ErrChzzkTokenExchange)
	}
	return token, nil
}

func (c *chzzkOAuthClient) ChannelForToken(ctx context.Context, accessToken string) (ChzzkChannel, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.usersMeURL, nil)
	if err != nil {
		return ChzzkChannel{}, err
	}
	// "Bearer" 뒤 공백 하나는 치지직이 엄격하게 요구한다(실측).
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	response, err := c.httpClient.Do(request)
	if err != nil {
		return ChzzkChannel{}, fmt.Errorf("request Chzzk user: %w", err)
	}
	defer response.Body.Close()
	content, code, message, err := decodeChzzkEnvelope(response)
	if err != nil {
		return ChzzkChannel{}, err
	}
	if code != http.StatusOK {
		return ChzzkChannel{}, fmt.Errorf("Chzzk user lookup failed: platform code %d: %s", code, message)
	}
	channel := ChzzkChannel{}
	if err := json.Unmarshal(content, &channel); err != nil {
		return ChzzkChannel{}, fmt.Errorf("decode Chzzk user: %w", err)
	}
	if strings.TrimSpace(channel.ID) == "" {
		return ChzzkChannel{}, ErrChzzkChannelMissing
	}
	return channel, nil
}

// decodeChzzkEnvelope는 공통 응답 봉투를 푼다. 치지직은 실패도 HTTP 200으로
// 주는 경우가 있어 HTTP 상태가 아니라 봉투의 code를 판정 근거로 쓴다.
// HTTP 상태가 5xx면 봉투가 아예 없을 수 있으므로 그때만 상태를 본다.
func decodeChzzkEnvelope(response *http.Response) (json.RawMessage, int, string, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, chzzkResponseLimit))
	if err != nil {
		return nil, 0, "", fmt.Errorf("read Chzzk response: %w", err)
	}
	envelope := chzzkEnvelope{}
	if err := json.Unmarshal(body, &envelope); err != nil {
		if response.StatusCode >= http.StatusInternalServerError {
			return nil, 0, "", fmt.Errorf("Chzzk returned HTTP %d", response.StatusCode)
		}
		return nil, 0, "", fmt.Errorf("decode Chzzk response: %w", err)
	}
	message := ""
	if envelope.Message != nil {
		message = *envelope.Message
	}
	return envelope.Content, envelope.Code, message, nil
}
