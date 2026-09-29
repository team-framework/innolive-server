package streaming

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// LiveUpdate는 방송 중에 송출을 끊지 않고 바꿀 수 있는 설정이다(#334). nil인
// 항목은 바꾸지 않는다. 플랫폼마다 쓰는 항목이 다르다 — 유튜브는 제목·설명·
// 카테고리, 치지직은 제목·카테고리(종류+식별자)·태그.
type LiveUpdate struct {
	Title        *string
	Description  *string
	CategoryID   *string
	CategoryType *string
	Tags         []string
	TagsSet      bool
}

// LiveUpdater는 진행 중인 방송의 설정을 바꿀 수 있는 플랫폼이다. 선택 구현이라
// 호출자는 타입 단언으로 지원 여부를 가린다.
type LiveUpdater interface {
	UpdateLive(ctx context.Context, userID uuid.UUID, prepared PreparedBroadcast, update LiveUpdate) error
}

// UpdateLive는 videos.update로 제목·설명·카테고리를 바꾼다. snippet은 통째
// 교체라 빠진 항목이 지워지므로(공식 문서) 현재 snippet을 읽어 바꿀 값만
// 덮어쓴다. liveBroadcasts.update는 공개 범위를 빠뜨리면 지워지는 등 필수
// 필드가 많아 쓰지 않는다.
func (p *YouTubeProvider) UpdateLive(ctx context.Context, userID uuid.UUID, prepared PreparedBroadcast, update LiveUpdate) error {
	if prepared.BroadcastID == "" {
		return errors.New("youtube live update requires a broadcast id")
	}
	accessToken, err := p.tokens.AccessToken(ctx, userID)
	if err != nil {
		return err
	}
	var current struct {
		Items []struct {
			Snippet map[string]any `json:"snippet"`
		} `json:"items"`
	}
	if err := p.do(ctx, accessToken, http.MethodGet, p.apiBase+"/videos?part=snippet&id="+prepared.BroadcastID, "", nil, &current); err != nil {
		return fmt.Errorf("read live video snippet: %w", err)
	}
	if len(current.Items) == 0 || current.Items[0].Snippet == nil {
		return fmt.Errorf("read live video snippet: video %s not found", prepared.BroadcastID)
	}
	snippet := current.Items[0].Snippet
	// 읽기 전용·파생 필드는 되돌려 보내지 않는다.
	for _, field := range []string{"publishedAt", "channelId", "channelTitle", "thumbnails", "liveBroadcastContent", "localized"} {
		delete(snippet, field)
	}
	if update.Title != nil {
		snippet["title"] = strings.TrimSpace(*update.Title)
	}
	if update.Description != nil {
		snippet["description"] = *update.Description
	}
	if update.CategoryID != nil {
		snippet["categoryId"] = strings.TrimSpace(*update.CategoryID)
	}
	payload := map[string]any{"id": prepared.BroadcastID, "snippet": snippet}
	if err := p.do(ctx, accessToken, http.MethodPut, p.apiBase+"/videos?part=snippet", "application/json", payload, &struct{}{}); err != nil {
		return fmt.Errorf("update live video snippet: %w", err)
	}
	return nil
}

// UpdateLive는 방송 준비 때와 같은 PATCH /open/v1/lives/setting이다. 문서가
// 진행 중 방송 반영 여부를 적지 않아 실채널로 확인한다(#334).
func (p *ChzzkProvider) UpdateLive(ctx context.Context, userID uuid.UUID, _ PreparedBroadcast, update LiveUpdate) error {
	accessToken, err := p.tokens.AccessToken(ctx, userID)
	if err != nil {
		return err
	}
	var options PrepareOptions
	if update.Title != nil {
		options.Title = *update.Title
	}
	if update.CategoryType != nil && update.CategoryID != nil {
		options.CategoryType, options.CategoryID = *update.CategoryType, *update.CategoryID
	}
	if update.TagsSet {
		options.Tags = append([]string{}, update.Tags...)
	}
	return p.applySetting(ctx, accessToken, options)
}
