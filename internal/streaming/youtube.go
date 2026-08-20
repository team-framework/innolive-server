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
	// 미디어 업로드는 별도 호스트다(thumbnails.set).
	youtubeUploadBase = "https://www.googleapis.com/upload/youtube/v3"
	// 기본 방송 속성. Privacy 기본값은 실수로 시작된 방송이 곧바로 전체공개로
	// 나가지 않도록 unlisted다. 전체공개는 start 요청에서 명시해야 한다.
	defaultBroadcastTitle   = "InnoLive 방송"
	defaultBroadcastPrivacy = "unlisted"
)

var (
	// ErrLiveStreamingBlocked: 채널이 라이브 활성화 대기(신청 후 최대 24시간)
	// 이거나 차단된 상태. 실측(2026-08-09 403 → 활성화 후 08-10 200)으로
	// reason=="livePermissionBlocked"가 이 상태의 시그니처임을 확인했다.
	ErrLiveStreamingBlocked = errors.New("the YouTube channel is not enabled for live streaming")
	// ErrThumbnailForbidden: 썸네일 업로드가 거부된 상태(채널 전화번호 인증
	// 미완료가 대표 원인). 선택 항목이므로 방송은 진행하고 경고로 알린다.
	ErrThumbnailForbidden = errors.New("the YouTube channel is not allowed to upload custom thumbnails")
	// ErrBroadcastNotReady: 라이브 전환 요건이 아직 안 갖춰진 상태.
	// transition(live)는 바인딩된 스트림이 active여야 허용된다.
	ErrBroadcastNotReady = errors.New("the YouTube broadcast is not ready to go live")
	// ErrMadeForKidsRequired: 시청자층(아동용 여부)은 YouTube가 요구하는
	// 법적 신고 항목이라 서버가 대신 추정하지 않는다 — 미선택이면 거절한다.
	ErrMadeForKidsRequired = errors.New("made_for_kids must be specified by the user")
	// ThumbnailHelpURL은 썸네일 업로드 권한(채널 인증) 안내 문서다.
	ThumbnailHelpURL = "https://support.google.com/youtube/answer/72431"
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
// 라이브 전환은 GoLive의 transition 호출이 담당하고(enableAutoStart=false),
// 종료만 enableAutoStop에 맡긴다(실측: 송출 중단 → 57.6초에 complete).
type YouTubeProvider struct {
	tokens     AccessTokenProvider
	store      auth.StreamingAccountStore
	cipher     *auth.ProviderTokenCipher
	httpClient *http.Client
	apiBase    string
	uploadBase string
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
		uploadBase: youtubeUploadBase,
		now:        func() time.Time { return time.Now().UTC() },
	}, nil
}

// Prepare는 재사용 스트림을 확보하고(첫 방송 때 1회 생성·저장, 이후 재사용),
// broadcast를 만들어 스트림에 바인딩한 뒤 ingest URL을 돌려준다. 방송은
// 시청자에게 노출되지 않는 ready 상태로 남고, 라이브 전환은 GoLive가 한다.
func (p *YouTubeProvider) Prepare(ctx context.Context, userID uuid.UUID, options PrepareOptions) (PreparedBroadcast, error) {
	if options.MadeForKids == nil {
		return PreparedBroadcast{}, ErrMadeForKidsRequired
	}
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
	// 여기까지가 필수 경로다. 카테고리·썸네일은 선택 항목이라 실패해도
	// 방송을 되돌리지 않고 경고로만 알린다.
	return PreparedBroadcast{
		Provider:    auth.StreamingProviderYouTube,
		IngestURL:   ingestAddress + "/" + streamName,
		BroadcastID: broadcastID,
		StreamID:    streamID,
		Warnings:    p.applyOptionalSettings(ctx, accessToken, broadcastID, options),
	}, nil
}

// Stop은 no-op이다: enableAutoStop이 송출 중단(egress 종료) 약 1분 후
// 방송을 complete로 전환한다(실측 57.6초). 즉시 종료가 필요해지면
// transition(complete) 호출을 여기 추가한다.
// GoLive는 준비된 방송을 라이브로 전환한다. 바인딩된 스트림에 실제 프레임이
// 도착해 active가 된 뒤에만 허용되므로, 아직 아니면 ErrBroadcastNotReady다.
func (p *YouTubeProvider) GoLive(ctx context.Context, userID uuid.UUID, prepared PreparedBroadcast) error {
	if prepared.BroadcastID == "" {
		return errors.New("prepared broadcast has no id")
	}
	accessToken, err := p.tokens.AccessToken(ctx, userID)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/liveBroadcasts/transition?broadcastStatus=live&id=%s&part=id,status", prepared.BroadcastID)
	return p.post(ctx, accessToken, path, nil, &struct{}{})
}

// Stop은 아직 라이브가 되지 않은 방송을 삭제한다. autoStart를 끈 뒤로는
// prepare만 하고 중지한 방송이 채널에 ready 상태로 남기 때문이다. 라이브였던
// 방송은 호출자가 여기까지 보내지 않고 autoStop에 맡긴다.
func (p *YouTubeProvider) Stop(ctx context.Context, userID uuid.UUID, prepared PreparedBroadcast) error {
	if prepared.BroadcastID == "" {
		return nil
	}
	accessToken, err := p.tokens.AccessToken(ctx, userID)
	if err != nil {
		return err
	}
	return p.do(ctx, accessToken, http.MethodDelete, p.apiBase+"/liveBroadcasts?id="+prepared.BroadcastID, "", nil, nil)
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

// resolveSnippet은 insert와 videos.update가 같은 제목·설명을 싣도록 한 곳에서
// 기본값을 채운다 — videos.update의 snippet은 통째 교체라 값이 어긋나면
// 방송 정보가 지워진다.
func resolveSnippet(options PrepareOptions) (title, description string) {
	title = strings.TrimSpace(options.Title)
	if title == "" {
		title = defaultBroadcastTitle
	}
	return title, options.Description
}

func (p *YouTubeProvider) insertBroadcast(ctx context.Context, accessToken string, options PrepareOptions) (string, error) {
	title, description := resolveSnippet(options)
	privacy := strings.TrimSpace(options.Privacy)
	if privacy == "" {
		privacy = defaultBroadcastPrivacy
	}
	payload := map[string]any{
		"snippet": map[string]any{
			"title":       title,
			"description": description,
			// 라이브 전환은 GoLive가 하므로 예정 시각은 형식 요건일 뿐이다.
			"scheduledStartTime": p.now().Format(time.RFC3339),
		},
		"status": map[string]any{
			"privacyStatus":           privacy,
			"selfDeclaredMadeForKids": *options.MadeForKids,
		},
		"contentDetails": map[string]any{
			// 준비와 라이브 전환을 분리하려고 autoStart를 끈다(#142) —
			// 켜져 있으면 egress가 붙는 즉시 방송이 시청자에게 공개된다.
			"enableAutoStart": false,
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

// applyOptionalSettings는 카테고리·썸네일을 반영하고, 실패한 항목만 경고로
// 돌려준다. 반환값이 비어 있으면 모두 반영된 것이다.
func (p *YouTubeProvider) applyOptionalSettings(ctx context.Context, accessToken, broadcastID string, options PrepareOptions) []Warning {
	var warnings []Warning
	if strings.TrimSpace(options.CategoryID) != "" {
		if err := p.updateVideoCategory(ctx, accessToken, broadcastID, options); err != nil {
			warnings = append(warnings, Warning{
				Code:    "category_not_applied",
				Message: "The broadcast category could not be set: " + err.Error(),
			})
		}
	}
	if options.Thumbnail != nil {
		if err := p.setThumbnail(ctx, accessToken, broadcastID, *options.Thumbnail); err != nil {
			warning := Warning{
				Code:    "thumbnail_not_applied",
				Message: "The thumbnail could not be uploaded: " + err.Error(),
			}
			if errors.Is(err, ErrThumbnailForbidden) {
				warning.Code = "thumbnail_forbidden"
				warning.Message = "Uploading a custom thumbnail requires a verified YouTube channel (phone verification). See " + ThumbnailHelpURL
			}
			warnings = append(warnings, warning)
		}
	}
	return warnings
}

// updateVideoCategory는 카테고리를 설정한다. liveBroadcasts에 카테고리가 없어
// videos.update를 쓰는데, snippet이 부분 갱신이 아니라 통째 교체라 제목·설명을
// 반드시 함께 싣는다.
func (p *YouTubeProvider) updateVideoCategory(ctx context.Context, accessToken, videoID string, options PrepareOptions) error {
	title, description := resolveSnippet(options)
	payload := map[string]any{
		"id": videoID,
		"snippet": map[string]any{
			"title":       title,
			"description": description,
			"categoryId":  strings.TrimSpace(options.CategoryID),
		},
	}
	return p.do(ctx, accessToken, http.MethodPut, p.apiBase+"/videos?part=snippet", "application/json", payload, &struct{}{})
}

// setThumbnail은 thumbnails.set 미디어 업로드다 — 이미지 바이트를 본문에
// 그대로 싣는 경로라 JSON 전용인 post로는 보낼 수 없다.
func (p *YouTubeProvider) setThumbnail(ctx context.Context, accessToken, videoID string, thumbnail Thumbnail) error {
	path := p.uploadBase + "/thumbnails/set?videoId=" + videoID + "&uploadType=media"
	err := p.do(ctx, accessToken, http.MethodPost, path, thumbnail.MIME, thumbnail.Data, &struct{}{})
	// 403은 채널 인증이 안 된 계정의 시그니처다 — 재시도 대상이 아니라
	// 사용자 안내로 바꿔야 하므로 여기서만 도메인 에러로 승격한다.
	var status apiStatusError
	if errors.As(err, &status) && status.status == http.StatusForbidden {
		return fmt.Errorf("%w: %s", ErrThumbnailForbidden, status.message)
	}
	return err
}

func (p *YouTubeProvider) post(ctx context.Context, accessToken, path string, payload, target any) error {
	return p.do(ctx, accessToken, http.MethodPost, p.apiBase+path, "application/json", payload, target)
}

// do는 YouTube API 호출 1회다. payload가 []byte면 본문에 그대로 싣고(미디어
// 업로드), 그 외에는 JSON으로 인코딩한다. nil이면 본문 없이 보낸다.
func (p *YouTubeProvider) do(ctx context.Context, accessToken, method, url, contentType string, payload, target any) error {
	var body io.Reader
	switch value := payload.(type) {
	case nil:
	case []byte:
		body = bytes.NewReader(value)
	default:
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	if body != nil {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := p.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("request YouTube Live API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeYouTubeAPIError(response)
	}
	if target == nil {
		// 본문 없는 성공 응답(liveBroadcasts.delete의 204).
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 256<<10)).Decode(target)
}

// apiStatusError는 도메인 에러로 분류되지 않은 API 실패다. 상태 코드를
// 남겨 호출자가 맥락에 맞게(예: 썸네일 403) 승격할 수 있게 한다.
type apiStatusError struct {
	status  int
	message string
}

func (e apiStatusError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("YouTube API returned HTTP %d", e.status)
	}
	return fmt.Sprintf("YouTube API returned HTTP %d (%s)", e.status, e.message)
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
		return apiStatusError{status: response.StatusCode}
	}
	for _, item := range payload.Error.Errors {
		switch item.Reason {
		case "livePermissionBlocked":
			return ErrLiveStreamingBlocked
		case "errorStreamInactive", "invalidTransition":
			// 스트림에 프레임이 아직 도착하지 않았거나 방송이 전환 가능한
			// 상태가 아니다 — 재시도로 풀리는 상태라 별도 에러로 구분한다.
			return ErrBroadcastNotReady
		}
	}
	return apiStatusError{status: response.StatusCode, message: payload.Error.Message}
}
