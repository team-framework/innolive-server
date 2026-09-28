package server

import (
	"context"
	"errors"
	"net/http"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/streaming"

	"github.com/google/uuid"
)

// youtubeCategoryLister는 방송 카테고리 목록을 주는 플랫폼이다. 유튜브만 해당한다
// (치지직은 검색 API가 따로 있다 — GET /auth/chzzk/categories).
type youtubeCategoryLister interface {
	Categories(ctx context.Context, userID uuid.UUID) ([]streaming.VideoCategory, error)
}

// handleGetYouTubeCategories는 연결된 유튜브 계정으로 방송에 고를 수 있는 카테고리를
// 돌려준다. 방송 설정 화면의 드롭다운용이다.
func (s *Server) handleGetYouTubeCategories(w http.ResponseWriter, r *http.Request) {
	lister, ok := s.streaming[auth.StreamingProviderYouTube].(youtubeCategoryLister)
	if !ok {
		writeError(w, apiError{Status: http.StatusNotImplemented, Code: "not_supported", Message: "Streaming to this platform is not configured on the server.", Details: map[string]any{"provider": auth.StreamingProviderYouTube}})
		return
	}
	userID, _ := auth.UserIDFromContext(r.Context())
	categories, err := lister.Categories(r.Context(), userID)
	if err != nil {
		if errors.Is(err, auth.ErrStreamingNotConnected) || errors.Is(err, auth.ErrStreamingReconnectRequired) || errors.Is(err, streaming.ErrPlatformRateLimited) {
			writeError(w, *s.prepareError(err, "", auth.StreamingProviderYouTube))
			return
		}
		s.logger.Error("list YouTube categories failed", "user_id", userID, "error", err)
		writeError(w, apiError{Status: http.StatusBadGateway, Code: "streaming_categories_failed", Message: "The streaming platform could not list categories."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": categories})
}
