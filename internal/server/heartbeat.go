package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"inno-live-server/internal/session"
	"inno-live-server/internal/usage"

	"github.com/google/uuid"
)

// HeartbeatStore는 온디바이스 직접 송출 하트비트 저장소다(#434). 프로덕션은
// usage.HeartbeatStore다.
type HeartbeatStore interface {
	SaveHeartbeat(ctx context.Context, heartbeat usage.Heartbeat) error
}

// SetHeartbeatStore는 하트비트 라우트를 붙인다. 서버 조립 단계에서 한 번만 호출한다.
func (s *Server) SetHeartbeatStore(store HeartbeatStore) {
	if store == nil || s.mux == nil {
		return
	}
	s.heartbeats = store
	s.mux.Handle("POST /sessions/{session_id}/heartbeat", s.requireUser(s.requireSessionOwner(s.handleHeartbeat)))
}

// heartbeatRequest는 판정과 검증에 쓰는 필드만 읽는다. 나머지 보고 항목은
// 원문 그대로 저장한다 — 클라이언트 실측에 따라 항목이 바뀌어도 서버 변경이
// 필요 없게 한다.
type heartbeatRequest struct {
	SentAt  *time.Time `json:"sent_at"`
	Targets []struct {
		Provider  string `json:"provider"`
		Connected *bool  `json:"connected"`
	} `json:"targets"`
	Video *struct {
		FPS           *float64 `json:"fps"`
		DroppedFrames *int64   `json:"dropped_frames"`
	} `json:"video"`
	Anonymization *struct {
		FailedFrames *int64 `json:"failed_frames"`
	} `json:"anonymization"`
	Platform string `json:"platform"`
	Model    string `json:"model"`
}

// missingField는 필수 항목 중 빠진 첫 필드 이름이다. 모두 있으면 빈 문자열이다.
func (r heartbeatRequest) missingField() string {
	switch {
	case r.SentAt == nil:
		return "sent_at"
	case len(r.Targets) == 0:
		return "targets"
	case r.Video == nil || r.Video.FPS == nil:
		return "video.fps"
	case r.Video.DroppedFrames == nil:
		return "video.dropped_frames"
	case r.Anonymization == nil || r.Anonymization.FailedFrames == nil:
		return "anonymization.failed_frames"
	case r.Platform != "ios" && r.Platform != "android":
		return "platform"
	case strings.TrimSpace(r.Model) == "":
		return "model"
	}
	for _, target := range r.Targets {
		if strings.TrimSpace(target.Provider) == "" {
			return "targets[].provider"
		}
		if target.Connected == nil {
			return "targets[].connected"
		}
	}
	return ""
}

// handleHeartbeat은 온디바이스 직접 송출 중인 앱의 상태 보고를 받는다(#434).
// 판정(입력 없음·한도 종료)은 #436에서 붙는다. 응답은 세션의 알림과 남은
// 시간을 함께 실어 앱이 세션을 따로 조회하지 않게 한다.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request, liveSession *session.Session) {
	if liveSession.AIProcessing != session.AIProcessingOnDevice {
		writeError(w, heartbeatNotApplicable(liveSession.ID, "Heartbeats are only accepted for on-device sessions."))
		return
	}
	if len(liveSession.DirectLiveTargets()) == 0 {
		writeError(w, heartbeatNotApplicable(liveSession.ID, "The session has no live broadcast target."))
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		writeError(w, badRequest("Invalid heartbeat request.", map[string]any{"error": err.Error()}))
		return
	}
	var request heartbeatRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		writeError(w, badRequest("Invalid heartbeat request.", map[string]any{"error": err.Error()}))
		return
	}
	if field := request.missingField(); field != "" {
		writeError(w, badRequest("Heartbeat field is missing or invalid.", map[string]any{"field": field}))
		return
	}
	sessionID, err := uuid.Parse(liveSession.ID)
	if err != nil {
		writeError(w, internalError())
		return
	}
	connected := 0
	for _, target := range request.Targets {
		if *target.Connected {
			connected++
		}
	}
	heartbeat := usage.Heartbeat{
		SessionID:        sessionID,
		ReceivedAt:       time.Now().UTC(),
		SentAt:           request.SentAt.UTC(),
		ConnectedTargets: connected,
		VideoFPS:         *request.Video.FPS,
		DroppedFrames:    *request.Video.DroppedFrames,
		FailedFrames:     *request.Anonymization.FailedFrames,
		Platform:         request.Platform,
		Payload:          string(raw),
	}
	if err := s.heartbeats.SaveHeartbeat(r.Context(), heartbeat); err != nil {
		s.logger.Error("save heartbeat failed", "session_id", liveSession.ID, "error", err)
		writeError(w, internalError())
		return
	}
	response := liveSession.Response()
	writeJSON(w, http.StatusOK, struct {
		StopTargets               []string         `json:"stop_targets"`
		Notices                   []session.Notice `json:"notices"`
		BroadcastRemainingSeconds *int64           `json:"broadcast_remaining_seconds"`
	}{StopTargets: []string{}, Notices: response.Notices, BroadcastRemainingSeconds: response.BroadcastRemainingSeconds})
}

func heartbeatNotApplicable(sessionID, message string) apiError {
	return apiError{Status: http.StatusConflict, Code: "heartbeat_not_applicable", Message: message, Details: map[string]any{"session_id": sessionID}}
}
