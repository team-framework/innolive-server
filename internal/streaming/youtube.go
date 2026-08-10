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
	youtubeAPIBase = "https://www.googleapis.com/youtube/v3"
	// 기본 방송 속성. Privacy 기본값 public은 "방송 시작을 명시적으로 요청한
	// 사용자의 기대치"를 따른 것으로, start 요청 옵션으로 바꿀 수 있다.
	defaultBroadcastTitle   = "InnoLive 방송"
	defaultBroadcastPrivacy = "public"
)

var (
	// ErrLiveStreamingBlocked: 채널이 라이브 활성화 대기(신청 후 최대 24시간)
	// 이거나 차단된 상태. 실측(2026-08-09 403 → 활성화 후 08-10 200)으로
	// reason=="livePermissionBlocked"가 이 상태의 시그니처임을 확인했다.
	ErrLiveStreamingBlocked = errors.New("the YouTube channel is not enabled for live streaming")
	// LiveStreamingHelpURL은 위 에러 응답의 extendedHelp 실물값 — 사용자
	// 안내에 그대로 쓴다.
	LiveStreamingHelpURL = "https://support.google.com/youtube/answer/2853834"
)

// AccessTokenProvider는 사용자별 플랫폼 access token 공급 계약이다.
// auth.YouTubeAccessTokenProvider가 이를 만족한다.
type AccessTokenProvider interface {
	AccessToken(ctx context.Context, userID uuid.UUID) (string, error)
}

// YouTubeProvider는 YouTube Live 방송 라이프사이클 구현이다.
// enableAutoStart/enableAutoStop을 사용하므로 transition 호출과 상태 폴링이
// 없다(실측: 송출 시작 → 13.7초에 live, 중단 → 57.6초에 complete).
type YouTubeProvider struct {
	tokens     AccessTokenProvider
	store      auth.StreamingAccountStore
	cipher     *auth.ProviderTokenCipher
	httpClient *http.Client
	apiBase    string
	now        func() time.Time
}

func NewYouTubeProvider(tokens AccessTokenProvider, store auth.StreamingAccountStore, cipher *auth.ProviderTokenCipher) (*YouTubeProvider, error) {
	if tokens == nil || store == nil || cipher == nil {
		return nil, errors.New("YouTube provider dependencies must not be nil")
	}
	return &YouTubeProvider{
		tokens:     tokens,
		store:      store,
		cipher:     cipher,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		apiBase:    youtubeAPIBase,
		now:        func() time.Time { return time.Now().UTC() },
	}, nil
}

// Prepare는 재사용 스트림을 확보하고(첫 방송 때 1회 생성·저장, 이후 재사용),
// autoStart/autoStop broadcast를 만들어 스트림에 바인딩한 뒤 ingest URL을
// 돌려준다.
func (p *YouTubeProvider) Prepare(ctx context.Context, userID uuid.UUID, options PrepareOptions) (PreparedBroadcast, error) {
	accessToken, err := p.tokens.AccessToken(ctx, userID)
	if err != nil {
		return PreparedBroadcast{}, err
	}
	account, err := p.store.Get(ctx, userID, auth.StreamingProviderYouTube)
	if err != nil {
		if errors.Is(err, auth.ErrStreamingAccountNotFound) {
			return PreparedBroadcast{}, auth.ErrStreamingNotConnected
		}
		return PreparedBroadcast{}, err
	}
	streamID, ingestAddress, streamName, err := p.ensureReusableStream(ctx, accessToken, account)
	if err != nil {
		return PreparedBroadcast{}, err
	}
	// 방송 준비 시점의 채널 표시 정보 갱신(#88 ④) — 조회 API가 저장값을
	// 반환하는 대가로 여기서 신선도를 맞춘다(1 unit). 부가 기능이므로 실패해도
	// 방송 준비는 계속한다.
	p.refreshChannelInfo(ctx, accessToken, account)
	broadcastID, err := p.insertBroadcast(ctx, accessToken, options)
	if err != nil {
		return PreparedBroadcast{}, err
	}
	if err := p.bind(ctx, accessToken, broadcastID, streamID); err != nil {
		return PreparedBroadcast{}, err
	}
	return PreparedBroadcast{
		Provider:    auth.StreamingProviderYouTube,
		IngestURL:   ingestAddress + "/" + streamName,
		BroadcastID: broadcastID,
		StreamID:    streamID,
	}, nil
}

// Stop은 no-op이다: enableAutoStop이 송출 중단(egress 종료) 약 1분 후
// 방송을 complete로 전환한다(실측 57.6초). 즉시 종료가 필요해지면
// transition(complete) 호출을 여기 추가한다.
func (p *YouTubeProvider) Stop(context.Context, uuid.UUID, PreparedBroadcast) error {
	return nil
}

// CleanupStreamingResources는 연결 해제 전에 프리로딩된 재사용 스트림을
// 플랫폼에서 삭제한다(#88 — DB 행만 지우면 사용자 채널에 고아 리소스가
// 남고 재연결마다 누적된다). 토큰이 이미 무효면 실패하는데, 호출자
// (StreamingAccountService)가 로그만 남기고 해제를 계속하는 계약이다.
func (p *YouTubeProvider) CleanupStreamingResources(ctx context.Context, account auth.StreamingAccount) error {
	if account.StreamID == nil || *account.StreamID == "" {
		return nil
	}
	accessToken, err := p.tokens.AccessToken(ctx, account.UserID)
	if err != nil {
		return fmt.Errorf("obtain access token for stream cleanup: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		p.apiBase+"/liveStreams?id="+*account.StreamID, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := p.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("request liveStreams.delete: %w", err)
	}
	defer response.Body.Close()
	// 204가 정상, 404는 이미 없는 것이라 목적 달성으로 본다.
	if response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusNotFound {
		return nil
	}
	return decodeYouTubeAPIError(response)
}

// ensureReusableStream은 계정에 저장된 재사용 스트림을 복호화해 돌려주고,
// 없으면 liveStreams.insert(isReusable=true)로 만들어 저장한다. 연결 시점이
// 아니라 첫 Prepare에서 lazy 생성하는 이유: 연결 서비스(auth)가 Live API에
// 의존하는 역방향 결합을 피하고, 라이브 미활성 계정도 연결 자체는 성공해야
// 하기 때문이다.
func (p *YouTubeProvider) ensureReusableStream(ctx context.Context, accessToken string, account auth.StreamingAccount) (streamID, ingestAddress, streamName string, err error) {
	if account.StreamID != nil && *account.StreamID != "" &&
		account.RtmpsIngestionAddress != nil && *account.RtmpsIngestionAddress != "" &&
		len(account.StreamNameCiphertext) > 0 {
		name, err := p.cipher.Decrypt(account.StreamNameCiphertext, account.StreamNameKeyVersion)
		if err != nil {
			return "", "", "", fmt.Errorf("decrypt stored stream name: %w", err)
		}
		return *account.StreamID, *account.RtmpsIngestionAddress, name, nil
	}

	payload := map[string]any{
		"snippet":        map[string]any{"title": "InnoLive stream"},
		"cdn":            map[string]any{"ingestionType": "rtmp", "resolution": "variable", "frameRate": "variable"},
		"contentDetails": map[string]any{"isReusable": true},
	}
	var response struct {
		ID  string `json:"id"`
		CDN struct {
			IngestionInfo struct {
				StreamName                  string `json:"streamName"`
				IngestionAddress            string `json:"ingestionAddress"`
				BackupIngestionAddress      string `json:"backupIngestionAddress"`
				RtmpsIngestionAddress       string `json:"rtmpsIngestionAddress"`
				RtmpsBackupIngestionAddress string `json:"rtmpsBackupIngestionAddress"`
			} `json:"ingestionInfo"`
		} `json:"cdn"`
	}
	if err := p.post(ctx, accessToken, "/liveStreams?part=snippet,cdn,contentDetails", payload, &response); err != nil {
		return "", "", "", err
	}
	info := response.CDN.IngestionInfo
	if response.ID == "" || info.RtmpsIngestionAddress == "" || info.StreamName == "" {
		return "", "", "", errors.New("liveStreams.insert response is missing ingestion info")
	}
	ciphertext, version, err := p.cipher.Encrypt(info.StreamName)
	if err != nil {
		return "", "", "", err
	}
	if err := p.store.UpdateStreamInfo(ctx, account.ID, auth.StreamInfo{
		StreamID:                    response.ID,
		IngestionAddress:            info.IngestionAddress,
		BackupIngestionAddress:      info.BackupIngestionAddress,
		RtmpsIngestionAddress:       info.RtmpsIngestionAddress,
		RtmpsBackupIngestionAddress: info.RtmpsBackupIngestionAddress,
		StreamNameCiphertext:        ciphertext,
		StreamNameKeyVersion:        version,
	}); err != nil {
		return "", "", "", fmt.Errorf("persist reusable stream info: %w", err)
	}
	return response.ID, info.RtmpsIngestionAddress, info.StreamName, nil
}

// refreshChannelInfo는 channels.list(mine=true)로 현재 채널 정보를 조회해
// 저장값과 다르면 갱신한다. 실패는 로그 대상도 아닌 무시다 — 표시 정보의
// 신선도일 뿐 방송 준비의 성패와 무관하다.
func (p *YouTubeProvider) refreshChannelInfo(ctx context.Context, accessToken string, account auth.StreamingAccount) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiBase+"/channels?part=snippet&mine=true", nil)
	if err != nil {
		return
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := p.httpClient.Do(request)
	if err != nil {
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
		return
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
		return
	}
	if len(payload.Items) == 0 || payload.Items[0].ID == "" {
		return
	}
	channelID := payload.Items[0].ID
	title := strings.TrimSpace(payload.Items[0].Snippet.Title)
	storedTitle := ""
	if account.ChannelTitle != nil {
		storedTitle = *account.ChannelTitle
	}
	if channelID == account.ChannelID && title == storedTitle {
		return
	}
	var titlePtr *string
	if title != "" {
		titlePtr = &title
	}
	_ = p.store.UpdateChannel(ctx, account.ID, channelID, titlePtr)
}

func (p *YouTubeProvider) insertBroadcast(ctx context.Context, accessToken string, options PrepareOptions) (string, error) {
	title := strings.TrimSpace(options.Title)
	if title == "" {
		title = defaultBroadcastTitle
	}
	privacy := strings.TrimSpace(options.Privacy)
	if privacy == "" {
		privacy = defaultBroadcastPrivacy
	}
	payload := map[string]any{
		"snippet": map[string]any{
			"title": title,
			// autoStart를 쓰므로 예정 시각은 형식 요건일 뿐이다(실측: 예정
			// 시각과 무관하게 송출 시작 13.7초 만에 live 전환).
			"scheduledStartTime": p.now().Format(time.RFC3339),
		},
		"status": map[string]any{
			"privacyStatus":           privacy,
			"selfDeclaredMadeForKids": false,
		},
		"contentDetails": map[string]any{
			"enableAutoStart": true,
			"enableAutoStop":  true,
			"monitorStream":   map[string]any{"enableMonitorStream": false},
		},
	}
	var response struct {
		ID string `json:"id"`
	}
	if err := p.post(ctx, accessToken, "/liveBroadcasts?part=snippet,contentDetails,status", payload, &response); err != nil {
		return "", err
	}
	if response.ID == "" {
		return "", errors.New("liveBroadcasts.insert response has no id")
	}
	return response.ID, nil
}

func (p *YouTubeProvider) bind(ctx context.Context, accessToken, broadcastID, streamID string) error {
	path := fmt.Sprintf("/liveBroadcasts/bind?id=%s&streamId=%s&part=id,contentDetails", broadcastID, streamID)
	return p.post(ctx, accessToken, path, nil, &struct{}{})
}

func (p *YouTubeProvider) post(ctx context.Context, accessToken, path string, payload, target any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := p.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("request YouTube Live API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeYouTubeAPIError(response)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 256<<10)).Decode(target)
}

// decodeYouTubeAPIError는 오류 본문의 reason으로 도메인 에러를 구분한다.
// 본문 구조는 2026-08-09 실측 응답(error.errors[].reason)을 따른다.
func decodeYouTubeAPIError(response *http.Response) error {
	var payload struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 32<<10)).Decode(&payload); err != nil {
		return fmt.Errorf("YouTube Live API returned HTTP %d", response.StatusCode)
	}
	for _, item := range payload.Error.Errors {
		if item.Reason == "livePermissionBlocked" {
			return ErrLiveStreamingBlocked
		}
	}
	return fmt.Errorf("YouTube Live API returned HTTP %d (%s)", response.StatusCode, payload.Error.Message)
}
