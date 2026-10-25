package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"inno-live-server/internal/session"
)

// 서버 교체 배포(#517): 새 프로세스를 다른 포트로 띄운 뒤, 옛 프로세스가 세션을 새
// 프로세스로 넘긴다. 두 단계로 나눈다.
//
//  1. prepare: 모든 세션을 새 프로세스에 재연결 대기로 받아 둔다. 옛 프로세스는 그대로
//     서비스한다. 이 뒤로 Caddy를 새 프로세스로 돌려도 세션 조회가 404가 되지 않는다.
//  2. commit: 최신 상태를 다시 보내고 옛 프로세스가 세션을 놓는다(PeerConnection·egress
//     종료). 클라이언트는 같은 세션 id로 새 프로세스와 다시 협상한다.

// handoffBusyRetry는 플랫폼 왕복 중인 세션을 commit에서 다시 시도하는 간격과 횟수다.
// 방송 준비·라이브 전환은 수 초 안에 끝난다.
const (
	handoffBusyRetryInterval = 500 * time.Millisecond
	handoffBusyRetryAttempts = 20
	handoffRequestTimeout    = 10 * time.Second
)

type sessionHandoffRequest struct {
	// To는 새 프로세스의 제어 API 주소다(예: http://127.0.0.1:9192). 스트림 키가
	// 오가므로 루프백 주소만 받는다.
	To    string `json:"to"`
	Phase string `json:"phase"`
}

type sessionHandoffResult struct {
	Phase    string            `json:"phase"`
	Handed   int               `json:"handed"`
	Detached int               `json:"detached"`
	Skipped  map[string]string `json:"skipped,omitempty"`
	Failed   map[string]string `json:"failed,omitempty"`
}

func (s *Server) handleSessionHandoff(w http.ResponseWriter, r *http.Request) {
	var request sessionHandoffRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil {
		writeAIControlJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"to\": \"http://127.0.0.1:port\", \"phase\": \"prepare|commit\"}"})
		return
	}
	target, err := loopbackControlURL(request.To)
	if err != nil {
		writeAIControlJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if request.Phase != "prepare" && request.Phase != "commit" {
		writeAIControlJSON(w, http.StatusBadRequest, map[string]string{"error": "phase must be prepare or commit"})
		return
	}
	result := s.handOffSessions(r.Context(), target, request.Phase == "commit")
	code := http.StatusOK
	if len(result.Failed) > 0 {
		code = http.StatusBadGateway
	}
	writeAIControlJSON(w, code, result)
}

// handOffSessions는 이 프로세스의 모든 세션을 target으로 넘긴다. commit이면 넘긴
// 세션을 놓는다. 넘기지 못한 세션은 그대로 이 프로세스에 남는다.
func (s *Server) handOffSessions(ctx context.Context, target *url.URL, commit bool) sessionHandoffResult {
	result := sessionHandoffResult{Phase: "prepare"}
	if commit {
		result.Phase = "commit"
	}
	for _, live := range s.sessions.List() {
		err := s.handOffSession(ctx, target, live.ID, commit)
		switch {
		case err == nil:
			result.Handed++
			if commit {
				result.Detached++
			}
		case errors.Is(err, session.ErrHandoffUnsupported), errors.Is(err, session.ErrNotFound):
			if result.Skipped == nil {
				result.Skipped = make(map[string]string)
			}
			result.Skipped[live.ID] = err.Error()
		default:
			if result.Failed == nil {
				result.Failed = make(map[string]string)
			}
			result.Failed[live.ID] = err.Error()
			s.logger.Warn("session handoff failed; the session stays on this server", "session_id", live.ID, "commit", commit, "error", err)
		}
	}
	s.logger.Info("session handoff finished", "phase", result.Phase, "handed", result.Handed, "skipped", len(result.Skipped), "failed", len(result.Failed))
	return result
}

func (s *Server) handOffSession(ctx context.Context, target *url.URL, id string, commit bool) error {
	var snapshot session.SessionSnapshot
	var err error
	for attempt := 0; ; attempt++ {
		snapshot, err = s.sessions.ExportSession(id)
		if !errors.Is(err, session.ErrHandoffBusy) || !commit || attempt >= handoffBusyRetryAttempts {
			break
		}
		select {
		case <-time.After(handoffBusyRetryInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	if err := s.postSessionImport(ctx, target, snapshot, commit); err != nil {
		return err
	}
	if !commit {
		return nil
	}
	return s.sessions.DetachSession(id)
}

func (s *Server) postSessionImport(ctx context.Context, target *url.URL, snapshot session.SessionSnapshot, commit bool) error {
	body, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	endpoint := target.JoinPath("sessions", "import")
	if commit {
		endpoint.RawQuery = "commit=true"
	}
	callCtx, cancel := context.WithTimeout(ctx, handoffRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+s.cfg.AIControlToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("post session import: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("session import returned %d: %s", response.StatusCode, bytes.TrimSpace(detail))
	}
	return nil
}

// maxSessionSnapshotBytes는 세션 스냅샷 하나의 상한이다. 설정·방송 상태뿐이라 수 KB다.
const maxSessionSnapshotBytes = 1 << 20

func (s *Server) handleSessionImport(w http.ResponseWriter, r *http.Request) {
	var snapshot session.SessionSnapshot
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSessionSnapshotBytes)).Decode(&snapshot); err != nil {
		writeAIControlJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid session snapshot"})
		return
	}
	commit := r.URL.Query().Get("commit") == "true"
	if err := s.sessions.ImportSession(snapshot, commit); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, session.ErrHandoffConflict) {
			code = http.StatusConflict
		}
		writeAIControlJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	// 이 프로세스가 만들지 않은 방송도 InnoLive 방송으로 센다(#361).
	for _, target := range snapshot.Targets {
		if target.PlatformBroadcast != nil && target.PlatformBroadcast.BroadcastID != "" {
			s.ownBroadcasts.add(target.PlatformBroadcast.BroadcastID)
		}
	}
	writeAIControlJSON(w, http.StatusOK, map[string]any{"session_id": snapshot.ID, "committed": commit})
}

// loopbackControlURL은 다른 프로세스의 제어 API 주소가 http://루프백IP:포트인지 확인한다.
func loopbackControlURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Path != "" && parsed.Path != "/" {
		return nil, errors.New("to must be http://<loopback ip>:<port>")
	}
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return nil, errors.New("to must include a port")
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, errors.New("to must be a loopback IP")
	}
	return &url.URL{Scheme: "http", Host: parsed.Host}, nil
}
