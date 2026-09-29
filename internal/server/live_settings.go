package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"
	"inno-live-server/internal/streaming"
)

// liveSettingsFields는 방송 중에 바꿀 수 있는 항목이다(#334). 공개 범위·아동용
// 신고·썸네일처럼 라이브 중에 바꾸면 안 되거나 못 바꾸는 항목은 받지 않는다.
var liveSettingsFields = map[auth.StreamingProvider]map[string]bool{
	auth.StreamingProviderYouTube: {"title": true, "description": true, "category_id": true},
	auth.StreamingProviderChzzk:   {"title": true, "category_type": true, "category_id": true, "tags": true},
}

// handlePatchLiveBroadcast는 송출을 끊지 않고 진행 중인 방송의 설정을 바꾼다.
// 플랫폼에 반영한 뒤에만 세션 저장값을 바꿔, 저장값과 실제 방송이 갈리지 않게 한다.
func (s *Server) handlePatchLiveBroadcast(w http.ResponseWriter, r *http.Request, liveSession *session.Session) {
	providerName, invalid := targetProviderFrom(r, liveSession)
	if invalid != nil {
		writeError(w, *invalid)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	raw := map[string]json.RawMessage{}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeError(w, badRequest("Invalid live broadcast settings request.", nil))
		return
	}
	allowed := liveSettingsFields[providerName]
	var rejected []string
	for field := range raw {
		if !allowed[field] {
			rejected = append(rejected, field)
		}
	}
	if len(rejected) > 0 {
		sort.Strings(rejected)
		writeError(w, apiError{Status: http.StatusBadRequest, Code: "field_not_changeable_live",
			Message: "These settings cannot be changed during a broadcast.", Details: map[string]any{"fields": rejected, "provider": providerName}})
		return
	}
	var request struct {
		Title        *string   `json:"title"`
		Description  *string   `json:"description"`
		CategoryID   *string   `json:"category_id"`
		CategoryType *string   `json:"category_type"`
		Tags         *[]string `json:"tags"`
	}
	body, _ := json.Marshal(raw)
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(w, badRequest("Invalid live broadcast settings request.", nil))
		return
	}
	if request.Title != nil && strings.TrimSpace(*request.Title) == "" {
		writeError(w, badRequest("Invalid broadcast settings request.", map[string]any{"field": "title", "reason": "must not be empty"}))
		return
	}

	// 저장값에 덮어쓴 결과로 검증한다 — 방송 준비와 같은 규칙(길이·<>·카테고리 쌍).
	update := streaming.LiveUpdate{Title: request.Title, Description: request.Description, CategoryID: request.CategoryID, CategoryType: request.CategoryType}
	var youtubeSettings session.YouTubeBroadcastSettings
	var chzzkSettings session.ChzzkBroadcastSettings
	var validation error
	if providerName == auth.StreamingProviderChzzk {
		chzzkSettings = liveSession.ChzzkBroadcastSettings()
		if request.Title != nil {
			chzzkSettings.Title = strings.TrimSpace(*request.Title)
		}
		if request.CategoryType != nil {
			chzzkSettings.CategoryType = strings.TrimSpace(*request.CategoryType)
		}
		if request.CategoryID != nil {
			chzzkSettings.CategoryID = strings.TrimSpace(*request.CategoryID)
		}
		if request.Tags != nil {
			chzzkSettings.Tags = *request.Tags
			update.Tags, update.TagsSet = *request.Tags, true
		}
		// 치지직 카테고리는 종류·식별자가 한 쌍이다 — 한쪽만 오면 저장값의 짝을 싣는다.
		if request.CategoryType != nil || request.CategoryID != nil {
			update.CategoryType, update.CategoryID = &chzzkSettings.CategoryType, &chzzkSettings.CategoryID
		}
		validation = chzzkSettings.Validate()
	} else {
		youtubeSettings = liveSession.BroadcastSettings()
		if request.Title != nil {
			youtubeSettings.Title = strings.TrimSpace(*request.Title)
		}
		if request.Description != nil {
			youtubeSettings.Description = *request.Description
		}
		if request.CategoryID != nil {
			youtubeSettings.CategoryID = strings.TrimSpace(*request.CategoryID)
		}
		validation = youtubeSettings.Validate()
	}
	var invalidSetting session.InvalidBroadcastSettingsError
	if errors.As(validation, &invalidSetting) {
		writeError(w, badRequest("Invalid broadcast settings request.", map[string]any{"field": invalidSetting.Field, "reason": invalidSetting.Reason}))
		return
	}

	if _, err := s.sessions.CheckLiveSettingsTarget(liveSession.ID, string(providerName)); err != nil {
		writeError(w, liveSettingsError(err, liveSession.ID, providerName))
		return
	}
	updater, ok := s.streaming[providerName].(streaming.LiveUpdater)
	if !ok {
		writeError(w, apiError{Status: http.StatusNotImplemented, Code: "not_supported", Message: "Changing settings during a broadcast is not supported for this platform.", Details: map[string]any{"provider": providerName}})
		return
	}
	broadcast, _ := liveSession.PlatformBroadcast(string(providerName))
	prepared := streaming.PreparedBroadcast{Provider: providerName, BroadcastID: broadcast.BroadcastID, StreamID: broadcast.StreamID}
	if err := updater.UpdateLive(r.Context(), liveSession.UserID, prepared, update); err != nil {
		writeError(w, s.liveUpdateError(err, liveSession.ID, providerName))
		return
	}

	var committed *session.Session
	var err error
	if providerName == auth.StreamingProviderChzzk {
		committed, err = s.sessions.CommitLiveChzzkBroadcastSettings(liveSession.ID, string(providerName), chzzkSettings)
	} else {
		committed, err = s.sessions.CommitLiveBroadcastSettings(liveSession.ID, string(providerName), youtubeSettings)
	}
	if err != nil {
		// 플랫폼에는 반영됐지만 그 사이 방송이 끝났다 — 다음 방송은 새 설정으로 연다.
		writeError(w, liveSettingsError(err, liveSession.ID, providerName))
		return
	}
	writeJSON(w, http.StatusOK, committed.Response())
}

func liveSettingsError(err error, sessionID string, providerName auth.StreamingProvider) apiError {
	if errors.Is(err, session.ErrBroadcastNotLive) {
		return apiError{Status: http.StatusConflict, Code: "broadcast_not_live", Message: "There is no prepared or live broadcast to update.", Details: map[string]any{"session_id": sessionID, "provider": providerName}}
	}
	return *sessionError(err, sessionID)
}

// liveUpdateError는 플랫폼 반영 실패를 옮긴다. 계정·한도 문제는 방송 준비와 같은
// 코드로, 나머지는 반영 실패(502)로 알린다.
func (s *Server) liveUpdateError(err error, sessionID string, providerName auth.StreamingProvider) apiError {
	if errors.Is(err, auth.ErrStreamingNotConnected) || errors.Is(err, auth.ErrStreamingReconnectRequired) || errors.Is(err, streaming.ErrPlatformRateLimited) {
		return *s.prepareError(err, sessionID, providerName)
	}
	s.logger.Error("live broadcast settings update failed", "session_id", sessionID, "provider", providerName, "error", err)
	return apiError{Status: http.StatusBadGateway, Code: "streaming_update_failed", Message: "The streaming platform could not apply the settings.", Details: map[string]any{"provider": providerName}}
}
