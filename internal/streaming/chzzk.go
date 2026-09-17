package streaming

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"inno-live-server/internal/auth"

	"github.com/google/uuid"
)

const (
	chzzkAPIBase = "https://openapi.chzzk.naver.com"
	// ChzzkIngestURL은 치지직 RTMP 수신 주소다. 스트림키 API가 주소를 주지
	// 않아(공개 문서 전수 확인) 스튜디오에 표시되는 값을 상수로 둔다. rtmps는
	// 제공되지 않는다(결정 D5: 평문 rtmp 수용).
	ChzzkIngestURL = "rtmp://global-rtmp.lip2.navercorp.com:8080/relay"
	// chzzkResponseLimit은 플랫폼 응답을 읽을 때의 상한이다.
	chzzkResponseLimit = 64 << 10
)

// ErrChzzkPlatform은 치지직 Open API가 봉투 code로 거절한 경우다. 메시지에는
// 플랫폼이 준 사유가 실리고 자격증명은 실리지 않는다.
var ErrChzzkPlatform = errors.New("Chzzk platform request failed")

// ChzzkProvider는 치지직 방송 라이프사이클 구현이다. 치지직에는 방송 객체가
// 없다 — RTMP 연결이 곧 방송 시작이고 끊으면 종료다(결정 D1). 그래서 Prepare는
// 설정 반영과 스트림키 조회까지만 하고, egress는 호출자가 GoLive 시점에
// 붙인다. GoLive·EndLive·Stop은 플랫폼에 할 일이 없어 no-op이다.
type ChzzkProvider struct {
	tokens     AccessTokenProvider
	httpClient *http.Client
	apiBase    string
	ingestURL  string
}

func NewChzzkProvider(tokens AccessTokenProvider) (*ChzzkProvider, error) {
	if tokens == nil {
		return nil, errors.New("Chzzk provider dependencies must not be nil")
	}
	return &ChzzkProvider{
		tokens:     tokens,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		apiBase:    chzzkAPIBase,
		ingestURL:  ChzzkIngestURL,
	}, nil
}

// chzzkLiveSetting은 GET·PATCH /open/v1/lives/setting의 content다. category는
// 미설정 채널에서 null로 온다.
type chzzkLiveSetting struct {
	DefaultLiveTitle string `json:"defaultLiveTitle"`
	Category         *struct {
		CategoryType string `json:"categoryType"`
		CategoryID   string `json:"categoryId"`
	} `json:"category"`
	Tags []string `json:"tags"`
}

// Prepare는 방송 설정을 채널에 반영하고 스트림키로 ingest URL을 조립한다.
// 설정은 채널 전역값(defaultLiveTitle)이라 되돌리지 않는다(결정 D4). 설정
// 반영 실패는 하드 에러다 — 제목이 곧 사용자가 요청한 방송이기 때문이다.
func (p *ChzzkProvider) Prepare(ctx context.Context, userID uuid.UUID, options PrepareOptions) (PreparedBroadcast, error) {
	accessToken, err := p.tokens.AccessToken(ctx, userID)
	if err != nil {
		return PreparedBroadcast{}, err
	}
	if err := p.applySetting(ctx, accessToken, options); err != nil {
		return PreparedBroadcast{}, err
	}
	var key struct {
		StreamKey string `json:"streamKey"`
	}
	if err := p.do(ctx, accessToken, http.MethodGet, "/open/v1/streams/key", nil, &key); err != nil {
		return PreparedBroadcast{}, fmt.Errorf("fetch stream key: %w", err)
	}
	if strings.TrimSpace(key.StreamKey) == "" {
		return PreparedBroadcast{}, fmt.Errorf("%w: stream key response is empty", ErrChzzkPlatform)
	}
	return PreparedBroadcast{
		Provider:  auth.StreamingProviderChzzk,
		IngestURL: p.ingestURL + "/" + key.StreamKey,
	}, nil
}

// applySetting은 PATCH /open/v1/lives/setting으로 제목·카테고리·태그를
// 반영한다. 전부 비어 있으면 채널 설정을 건드릴 이유가 없어 호출하지 않는다.
func (p *ChzzkProvider) applySetting(ctx context.Context, accessToken string, options PrepareOptions) error {
	body := map[string]any{}
	if title := strings.TrimSpace(options.Title); title != "" {
		body["defaultLiveTitle"] = title
	}
	if options.CategoryType != "" && options.CategoryID != "" {
		body["categoryType"] = options.CategoryType
		body["categoryId"] = options.CategoryID
	}
	if options.Tags != nil {
		body["tags"] = options.Tags
	}
	if len(body) == 0 {
		return nil
	}
	if err := p.do(ctx, accessToken, http.MethodPatch, "/open/v1/lives/setting", body, nil); err != nil {
		return fmt.Errorf("apply live setting: %w", err)
	}
	return nil
}

// GoLive는 no-op이다. 호출자가 이 직후 egress를 붙이고, 그 RTMP 연결이
// 치지직에서는 방송 시작이다.
func (p *ChzzkProvider) GoLive(context.Context, uuid.UUID, PreparedBroadcast) error { return nil }

// EndLive는 no-op이다. egress 종료가 곧 방송 종료다(유예는 플랫폼이 정한다).
func (p *ChzzkProvider) EndLive(context.Context, uuid.UUID, PreparedBroadcast) error { return nil }

// Stop은 no-op이다. 준비 단계에서는 플랫폼에 만들어진 것이 없다.
func (p *ChzzkProvider) Stop(context.Context, uuid.UUID, PreparedBroadcast) error { return nil }

// Defaults는 채널의 현재 방송 설정을 돌려준다. 유튜브와 달리 진짜 기본값을
// 읽는 API가 있다.
func (p *ChzzkProvider) Defaults(ctx context.Context, userID uuid.UUID) (BroadcastDefaults, error) {
	accessToken, err := p.tokens.AccessToken(ctx, userID)
	if err != nil {
		return BroadcastDefaults{}, err
	}
	var setting chzzkLiveSetting
	if err := p.do(ctx, accessToken, http.MethodGet, "/open/v1/lives/setting", nil, &setting); err != nil {
		return BroadcastDefaults{}, fmt.Errorf("fetch live setting: %w", err)
	}
	defaults := BroadcastDefaults{Title: setting.DefaultLiveTitle, Tags: setting.Tags}
	if setting.Category != nil {
		defaults.CategoryType = setting.Category.CategoryType
		defaults.CategoryID = setting.Category.CategoryID
	}
	if strings.TrimSpace(defaults.Title) == "" {
		defaults.Title = defaultBroadcastTitle
	}
	return defaults, nil
}

// do는 Open API를 부르고 봉투를 푼다. 치지직은 실패도 HTTP 200으로 주는
// 경우가 있어 봉투의 code를 판정 근거로 쓴다. 봉투 401은 토큰이 더 이상
// 유효하지 않다는 뜻이라 재연결 안내로 잇는다. URL·본문에 스트림키가 실리지
// 않으므로 에러 문자열에 경로를 남겨도 된다.
func (p *ChzzkProvider) do(ctx context.Context, accessToken, method, path string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, p.apiBase+path, body)
	if err != nil {
		return err
	}
	// "Bearer" 뒤 공백 하나는 치지직이 엄격하게 요구한다(실측).
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := p.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, chzzkResponseLimit))
	if err != nil {
		return fmt.Errorf("read %s %s: %w", method, path, err)
	}
	envelope := struct {
		Code    int             `json:"code"`
		Message *string         `json:"message"`
		Content json.RawMessage `json:"content"`
	}{}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		if response.StatusCode >= http.StatusInternalServerError {
			return fmt.Errorf("%w: %s %s returned HTTP %d", ErrChzzkPlatform, method, path, response.StatusCode)
		}
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	if envelope.Code != http.StatusOK {
		message := ""
		if envelope.Message != nil {
			message = *envelope.Message
		}
		if envelope.Code == http.StatusUnauthorized {
			return fmt.Errorf("%w: %s %s: %s", auth.ErrStreamingReconnectRequired, method, path, message)
		}
		return fmt.Errorf("%w: %s %s: platform code %d: %s", ErrChzzkPlatform, method, path, envelope.Code, message)
	}
	if out == nil || len(envelope.Content) == 0 || string(envelope.Content) == "null" {
		return nil
	}
	if err := json.Unmarshal(envelope.Content, out); err != nil {
		return fmt.Errorf("decode %s %s content: %w", method, path, err)
	}
	return nil
}
