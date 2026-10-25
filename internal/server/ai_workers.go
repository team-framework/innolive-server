package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"inno-live-server/internal/ai"
)

// AI 워커 교체(#515): 배포 스크립트가 새 워커 프로세스를 띄운 뒤 이 API로 옛 워커를
// 대신하게 한다. 진행 중인 방송은 파이프라인 재시작 없이 다음 프레임부터 새 워커로
// 넘어간다. 같은 호스트의 배포 스크립트만 부르므로 별도 루프백 리스너에서만 연다.

var (
	errAIWorkerNotFound = errors.New("AI worker is not in the pool")
	errAIWorkerSame     = errors.New("from and to must be different AI workers")
)

// aiWorkerReplacement는 워커 교체 한 번의 결과다.
type aiWorkerReplacement struct {
	From string `json:"from"`
	To   string `json:"to"`
	// CopiedSessions는 화이트리스트를 옮긴 AI 세션 수다.
	CopiedSessions int `json:"copied_sessions"`
	// PrunedEntries는 복사 중 지워진 얼굴이라 새 워커에서 다시 지운 엔트리 수다.
	PrunedEntries int `json:"pruned_entries"`
	MovedSessions int `json:"moved_sessions"`
	// FailedSessions는 넘기지 못해 옛 워커에 남은 세션 id와 사유다.
	FailedSessions map[string]string `json:"failed_sessions,omitempty"`
	// Removed는 옛 워커를 풀에서 뺐는지다. 남은 세션이 있으면 배정 제외로만 두며, 같은
	// 요청을 다시 보내면 남은 세션만 다시 옮긴다.
	Removed bool `json:"removed"`
}

// replaceAIWorker는 from 워커를 to 워커로 바꾼다.
//
//  1. to를 배정 제외로 풀에 더하고 프레임 왕복 preflight로 예열한다.
//  2. from의 화이트리스트를 엔트리 id 그대로 to에 옮기고 기준 얼굴 기록에도 적는다.
//  3. to의 배정을 열고 from을 배정 제외로 둔 뒤, from의 세션을 다음 프레임부터 넘긴다.
//  4. from에 남은 세션이 없으면 풀에서 뺀다.
//
// 2단계까지 실패하면 to를 풀에서 빼 처음 상태로 돌린다. 이미 to가 풀에 있으면(앞선
// 요청이 3단계에서 멈춤) 이어서 진행한다.
func (s *Server) replaceAIWorker(ctx context.Context, from, to string) (aiWorkerReplacement, error) {
	result := aiWorkerReplacement{From: from, To: to}
	if from == to {
		return result, errAIWorkerSame
	}
	if s.ai.Target(from) == nil {
		return result, fmt.Errorf("%w: %s", errAIWorkerNotFound, from)
	}
	target := s.ai.Target(to)
	added := false
	if target == nil {
		var err error
		if target, err = s.ai.AddTarget(to, s.ai.ServesFaceSwap(from)); err != nil {
			return result, err
		}
		added = true
	}
	abort := func(err error) (aiWorkerReplacement, error) {
		if added {
			_ = s.ai.RemoveTarget(to)
			if dropErr := s.references.dropWorkerEntries(to); dropErr != nil {
				s.logger.Warn("drop reference records of aborted AI worker failed", "target", to, "error", dropErr)
			}
		}
		return result, err
	}

	preflightCtx, cancel := context.WithTimeout(ctx, s.cfg.AIPreflightTimeout)
	err := target.Preflight(preflightCtx, string(s.cfg.AIWireFormat), s.cfg.DecoderPinLongEdge)
	cancel()
	if err != nil {
		return abort(fmt.Errorf("preflight %s: %w", to, err))
	}
	s.ai.SetTargetReady(to, true)

	// 기록을 먼저 적어, 복사하는 사이 들어온 얼굴 삭제가 to에도 닿게 한다.
	if err := s.references.copyWorkerEntries(from, to); err != nil {
		return abort(err)
	}
	copied, err := s.ai.CopyWhitelists(ctx, from, to)
	if err != nil {
		return abort(err)
	}
	result.CopiedSessions = len(copied)
	if result.PrunedEntries, err = s.pruneCopiedWhitelists(ctx, from, to); err != nil {
		return abort(err)
	}

	if err := s.ai.SetDraining(to, false); err != nil {
		return abort(err)
	}
	if err := s.ai.SetDraining(from, true); err != nil {
		return result, err
	}
	s.logger.Info("AI worker replacement opened new worker", "from", from, "to", to, "copied_sessions", result.CopiedSessions)

	// 배정 제외 직전에 from을 받은 세션이 아직 파이프라인을 띄우는 중일 수 있어 한
	// 번 더 본다.
	for attempt := 0; attempt < 2 && activeStreams(s.ai, from) > 0; attempt++ {
		report := s.sessions.SwitchSessionsOffAIWorker(ctx, from)
		result.MovedSessions += report.Moved
		result.FailedSessions = report.Failed
		if len(report.Failed) > 0 {
			break
		}
	}
	if activeStreams(s.ai, from) > 0 {
		s.logger.Warn("AI worker replacement left sessions on the old worker", "from", from, "to", to, "failed", result.FailedSessions)
		return result, nil
	}
	if err := s.ai.RemoveTarget(from); err != nil {
		return result, err
	}
	result.Removed = true
	if err := s.references.dropWorkerEntries(from); err != nil {
		s.logger.Warn("drop reference records of removed AI worker failed", "target", from, "error", err)
	}
	s.logger.Info("AI worker replaced", "from", from, "to", to, "moved_sessions", result.MovedSessions)
	return result, nil
}

func activeStreams(pool *ai.Pool, address string) int {
	for _, target := range pool.Targets() {
		if target.Address == address {
			return target.Active
		}
	}
	return 0
}

// pruneCopiedWhitelists는 복사하는 사이 지워진 얼굴을 to에서 지운다. to의 엔트리 중
// from에 지금 없고 서버 기록에도 to의 엔트리로 없는 것이 그 얼굴이다 — 지워진 얼굴이
// 새 워커에 남으면 그 사람이 블러되지 않는다. 막 등록 중인 얼굴을 잘못 지우면 대조가
// 재등록 필요로 표시하므로, 어긋나도 블러되는 쪽이다.
func (s *Server) pruneCopiedWhitelists(ctx context.Context, from, to string) (int, error) {
	source, target := s.ai.Target(from), s.ai.Target(to)
	if source == nil || target == nil {
		return 0, errAIWorkerNotFound
	}
	sessions, err := target.ListSessions(ctx)
	if err != nil {
		return 0, err
	}
	recorded := s.references.workerEntryIDs(to)
	pruned := 0
	for _, info := range sessions.GetSessions() {
		if info.GetEntryCount() == 0 {
			continue
		}
		sessionID := info.GetSessionId()
		current, err := target.GetWhitelistStatus(ctx, sessionID)
		if err != nil {
			return pruned, err
		}
		keep := make(map[string]bool)
		for _, entryID := range recorded[sessionID] {
			keep[entryID] = true
		}
		previous, err := source.GetWhitelistStatus(ctx, sessionID)
		if err != nil {
			return pruned, err
		}
		for _, entryID := range previous.GetEntryIds() {
			keep[entryID] = true
		}
		for _, entryID := range current.GetEntryIds() {
			if keep[entryID] {
				continue
			}
			if _, err := target.DeleteWhitelist(ctx, sessionID, entryID); err != nil {
				return pruned, err
			}
			pruned++
		}
	}
	return pruned, nil
}

// copyWorkerEntries는 from 워커의 엔트리 id 기록을 to 워커에도 적는다. 화이트리스트를
// 엔트리 id 그대로 옮기므로 같은 id다. 이미 to 기록이 있는 얼굴은 건드리지 않는다.
func (s *referenceStore) copyWorkerEntries(from, to string) error {
	return s.updateEntryIDs(func(entryIDs map[string]string) bool {
		if entryIDs[from] == "" || entryIDs[to] != "" {
			return false
		}
		entryIDs[to] = entryIDs[from]
		return true
	})
}

// dropWorkerEntries는 풀에서 빠진 워커의 엔트리 id 기록을 지운다.
func (s *referenceStore) dropWorkerEntries(address string) error {
	return s.updateEntryIDs(func(entryIDs map[string]string) bool {
		if _, ok := entryIDs[address]; !ok {
			return false
		}
		delete(entryIDs, address)
		return true
	})
}

// updateEntryIDs는 얼굴마다 엔트리 id 맵의 사본에 change를 적용하고, 바뀐 것이
// 있으면 저장한다. snapshot이 맵을 공유하므로 제자리에서 고치지 않는다.
func (s *referenceStore) updateEntryIDs(change func(map[string]string) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, faces := range s.faces {
		for index := range faces {
			entryIDs := maps.Clone(faces[index].EntryIDs)
			if entryIDs == nil {
				entryIDs = make(map[string]string)
			}
			if change(entryIDs) {
				faces[index].EntryIDs = entryIDs
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return s.save()
}

// workerEntryIDs는 클라이언트별로 address 워커에 기록된 엔트리 id다.
func (s *referenceStore) workerEntryIDs(address string) map[string][]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string][]string)
	for clientID, faces := range s.faces {
		for _, face := range faces {
			if entryID := face.EntryIDs[address]; entryID != "" {
				result[clientID] = append(result[clientID], entryID)
			}
		}
	}
	return result
}

// aiControlReplaceTimeout은 워커 교체 요청 하나의 한계다. 세션마다 예열이 몇 초씩
// 걸리고 동시에 두 개씩만 넘기므로, 워커 하나(최대 열 개 남짓)를 비우기에 넉넉하다.
const aiControlReplaceTimeout = 5 * time.Minute

// AIControlHandler는 AI 워커 교체 제어 API다. AI_CONTROL_ADDR의 루프백 리스너에만
// 붙이고, 모든 요청에 AI_CONTROL_TOKEN Bearer 토큰을 요구한다.
func (s *Server) AIControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ai/workers", s.handleListAIWorkers)
	mux.HandleFunc("POST /ai/workers/replace", s.handleReplaceAIWorker)
	return s.requireAIControlToken(mux)
}

func (s *Server) requireAIControlToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || s.cfg.AIControlToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AIControlToken)) != 1 {
			writeAIControlJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleListAIWorkers(w http.ResponseWriter, _ *http.Request) {
	if s.ai == nil {
		writeAIControlJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "AI is disabled"})
		return
	}
	writeAIControlJSON(w, http.StatusOK, map[string]any{"workers": s.ai.Targets()})
}

func (s *Server) handleReplaceAIWorker(w http.ResponseWriter, r *http.Request) {
	if s.ai == nil {
		writeAIControlJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "AI is disabled"})
		return
	}
	var request struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil || request.From == "" || request.To == "" {
		writeAIControlJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"from\": \"host:port\", \"to\": \"host:port\"}"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), aiControlReplaceTimeout)
	defer cancel()
	result, err := s.replaceAIWorker(ctx, request.From, request.To)
	if err != nil {
		code := http.StatusBadGateway
		switch {
		case errors.Is(err, errAIWorkerNotFound):
			code = http.StatusNotFound
		case errors.Is(err, errAIWorkerSame):
			code = http.StatusBadRequest
		case errors.Is(err, ai.ErrEmbeddingModelMismatch):
			code = http.StatusConflict
		}
		s.logger.Warn("AI worker replacement failed", "from", request.From, "to", request.To, "error", err)
		writeAIControlJSON(w, code, map[string]any{"error": err.Error(), "result": result})
		return
	}
	code := http.StatusOK
	if !result.Removed {
		// 남은 세션이 있어 옛 워커를 아직 끄면 안 된다.
		code = http.StatusAccepted
	}
	writeAIControlJSON(w, code, result)
}

func writeAIControlJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
