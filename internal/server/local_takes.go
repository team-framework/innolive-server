package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"inno-live-server/internal/auth"
	"inno-live-server/internal/usage"

	"github.com/google/uuid"
)

// LocalTakeRecorder는 iOS 로컬 모드 촬영 기록 저장소다. 프로덕션은 usage.Ledger다.
type LocalTakeRecorder interface {
	RecordLocalTakes(ctx context.Context, takes []usage.LocalTake) error
}

const (
	maxLocalTakesPerRequest = 100
	maxLocalTakeDuration    = 24 * time.Hour
	maxLocalDeviceIDLength  = 64
)

// SetLocalTakeRecorder는 로컬 촬영 업로드 라우트를 붙인다. 서버 조립 단계에서 한 번만 호출한다.
func (s *Server) SetLocalTakeRecorder(recorder LocalTakeRecorder) {
	if recorder == nil || s.mux == nil {
		return
	}
	s.mux.Handle("POST /users/me/usage/local-takes", s.requireUser(s.withUserOperation(localTakesHandler(recorder, s))))
}

type localTakeRequest struct {
	TakeID          string    `json:"take_id"`
	DeviceID        string    `json:"device_id"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at"`
	DurationSeconds int64     `json:"duration_seconds"`
}

type localTakesRequest struct {
	Takes []localTakeRequest `json:"takes"`
}

type localTakesResponse struct {
	// TakeIDs는 저장했거나 이미 있던 take_id다. 앱은 이 목록을 대기열에서 지운다.
	TakeIDs []uuid.UUID `json:"take_ids"`
}

// parseLocalTakes는 요청을 저장할 행으로 바꾼다. 실패하면 문제 필드 이름을 돌려준다.
// 같은 요청 안의 중복 take_id는 한 번만 남긴다.
func parseLocalTakes(request localTakesRequest, userID uuid.UUID, now time.Time) ([]usage.LocalTake, string) {
	if len(request.Takes) == 0 || len(request.Takes) > maxLocalTakesPerRequest {
		return nil, "takes"
	}
	takes := make([]usage.LocalTake, 0, len(request.Takes))
	seen := map[uuid.UUID]bool{}
	for i, raw := range request.Takes {
		field := func(name string) string { return fmt.Sprintf("takes[%d].%s", i, name) }
		takeID, err := uuid.Parse(raw.TakeID)
		if err != nil || takeID == uuid.Nil {
			return nil, field("take_id")
		}
		deviceID := strings.TrimSpace(raw.DeviceID)
		if deviceID == "" || len(deviceID) > maxLocalDeviceIDLength {
			return nil, field("device_id")
		}
		if raw.StartedAt.IsZero() {
			return nil, field("started_at")
		}
		span := raw.EndedAt.Sub(raw.StartedAt)
		if span <= 0 || span > maxLocalTakeDuration {
			return nil, field("ended_at")
		}
		// 앱은 초 단위로 반올림하므로 1초까지 구간보다 길 수 있다.
		if raw.DurationSeconds <= 0 || time.Duration(raw.DurationSeconds)*time.Second > span+time.Second {
			return nil, field("duration_seconds")
		}
		if seen[takeID] {
			continue
		}
		seen[takeID] = true
		user := userID
		takes = append(takes, usage.LocalTake{
			TakeID: takeID, UserID: &user, DeviceID: deviceID,
			StartedAt: raw.StartedAt.UTC(), EndedAt: raw.EndedAt.UTC(),
			DurationSeconds: raw.DurationSeconds, ReceivedAt: now,
		})
	}
	return takes, ""
}

func localTakesHandler(recorder LocalTakeRecorder, s *Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.UserIDFromContext(r.Context())
		if userID == uuid.Nil {
			writeError(w, apiError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "Authentication is required."})
			return
		}
		var request localTakesRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&request); err != nil {
			writeError(w, badRequest("Request body must be JSON.", nil))
			return
		}
		takes, field := parseLocalTakes(request, userID, time.Now().UTC())
		if field != "" {
			writeError(w, badRequest("Invalid local take.", map[string]any{"field": field}))
			return
		}
		if err := recorder.RecordLocalTakes(r.Context(), takes); err != nil {
			s.logger.Error("record local takes failed", "user_id", userID, "count", len(takes), "error", err)
			writeError(w, internalError())
			return
		}
		response := localTakesResponse{TakeIDs: make([]uuid.UUID, len(takes))}
		for i, take := range takes {
			response.TakeIDs[i] = take.TakeID
		}
		writeJSON(w, http.StatusOK, response)
	})
}
