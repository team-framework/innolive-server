package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"inno-live-server/internal/media"
	"inno-live-server/internal/session"
)

// handlePutMosaicConfig는 송출을 끊지 않고 블러 강도·영역을 바꾼다(#483).
func (s *Server) handlePutMosaicConfig(w http.ResponseWriter, r *http.Request, liveSession *session.Session) {
	s.putMosaicConfig(w, r, liveSession)
}

func (s *Server) handleGuestPutMosaicConfig(w http.ResponseWriter, r *http.Request) {
	live, _, ok := s.guestSessionRequest(w, r)
	if !ok {
		return
	}
	s.putMosaicConfig(w, r, live)
}

// putMosaicConfig는 본문을 세션 설정 전체로 받는다. 빠지거나 null인 필드는 AI
// 기본값이고 0과 다르다. 음수·소수·문자열은 디코딩에서, 범위 밖 값은 세션
// 매니저 검증에서 거절해 AI에 닿지 않는다.
func (s *Server) putMosaicConfig(w http.ResponseWriter, r *http.Request, liveSession *session.Session) {
	var cfg media.MosaicConfig
	if err := decodeOptionalJSON(http.MaxBytesReader(w, r.Body, maxJSONBody), &cfg); err != nil {
		details := map[string]any{"error": err.Error()}
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			details["field"] = typeErr.Field
		}
		writeError(w, badRequest("Invalid mosaic config request.", details))
		return
	}
	if _, err := s.sessions.SetMosaicConfig(liveSession.ID, cfg); err != nil {
		var invalid *media.MosaicConfigError
		if errors.As(err, &invalid) {
			writeError(w, badRequest("Invalid mosaic config request.", map[string]any{"field": invalid.Field, "error": invalid.Error()}))
			return
		}
		writeSessionError(w, err, liveSession.ID)
		return
	}
	writeJSON(w, http.StatusOK, liveSession.Response())
}
