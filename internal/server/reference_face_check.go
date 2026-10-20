package server

import (
	"context"
	"maps"
	"slices"
	"time"
)

// referenceCheckInterval은 서버 기록과 AI 워커 화이트리스트를 대조하는 간격이다.
// 워커는 화이트리스트를 메모리에만 두어 재시작하면 등록 얼굴을 잃는데, 서버 기록은
// 그대로 남아 등록됨으로 보였다(#501). 대조는 클라이언트·워커마다 메타데이터 읽기
// 한 번이라, 앱이 방송 전에 재등록 안내를 놓치지 않도록 짧게 둔다.
var referenceCheckInterval = time.Minute

// referenceCheckTimeout은 대조 한 바퀴의 한계다. 답하지 못한 워커는 이번 바퀴에서
// "모름"으로 남는다.
const referenceCheckTimeout = 30 * time.Second

// referenceFaceVerdict는 얼굴 하나를 워커 답과 견준 결과다.
type referenceFaceVerdict struct {
	// missing은 답한 워커 중 이 얼굴의 엔트리를 갖지 않은 워커 주소다.
	missing []string
	// complete는 모든 워커가 이 클라이언트에 대해 답했는지다.
	complete bool
}

// judgeReferenceFace는 얼굴 하나가 워커마다 남아 있는지 판정한다. observed는
// ai.Pool.WhitelistEntryIDs의 결과(워커 주소 → 클라이언트 → 엔트리 id)다.
//
// 이 워커의 엔트리 id가 기록에 없으면 빠진 것으로 친다. 그 워커가 풀에 들어오기 전에
// 등록했거나 워커별 id를 기록하기 전(2026-08 이전)에 등록한 얼굴이라, 그 워커는
// 얼굴을 가진 적이 없거나 이미 재시작으로 잃었다.
func judgeReferenceFace(face referenceFace, clientID string, observed map[string]map[string][]string) referenceFaceVerdict {
	verdict := referenceFaceVerdict{complete: true}
	for address, answers := range observed {
		entries, answered := answers[clientID]
		if !answered {
			verdict.complete = false
			continue
		}
		entryID := face.EntryIDs[address]
		if entryID == "" || !slices.Contains(entries, entryID) {
			verdict.missing = append(verdict.missing, address)
		}
	}
	slices.Sort(verdict.missing)
	return verdict
}

// referenceMark는 대조가 정한 얼굴 하나의 재등록 필요 여부다.
type referenceMark struct {
	clientID string
	// face는 대조한 시점의 기록이다. 반영할 때 기록이 그새 바뀌었는지 가리는 데 쓴다.
	face     referenceFace
	required bool
	missing  []string
}

// snapshot은 대조할 클라이언트별 얼굴 기록의 사본이다.
func (s *referenceStore) snapshot() map[string][]referenceFace {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string][]referenceFace, len(s.faces))
	for clientID, faces := range s.faces {
		result[clientID] = append([]referenceFace(nil), faces...)
	}
	return result
}

// face는 클라이언트의 얼굴 하나의 사본이다.
func (s *referenceStore) face(clientID, faceID string) (referenceFace, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, face := range s.faces[clientID] {
		if face.FaceID == faceID {
			return face, true
		}
	}
	return referenceFace{}, false
}

// applyMarks는 재등록 필요 표시를 기록에 반영하고 실제로 바뀐 표시를 돌려준다.
// 표시를 정한 뒤 지워졌거나 다시 등록된 얼굴(같은 FaceID가 없거나 EntryIDs가 달라진
// 기록)은 건드리지 않는다 — 그 판정은 이미 낡았다. 저장에 실패해도 메모리의 표시는
// 남긴다. 조회 응답은 메모리를 읽고, 다음 기동의 첫 대조가 표시를 다시 정한다.
func (s *referenceStore) applyMarks(marks []referenceMark) ([]referenceMark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var changed []referenceMark
	for _, mark := range marks {
		faces := s.faces[mark.clientID]
		index := slices.IndexFunc(faces, func(face referenceFace) bool { return face.FaceID == mark.face.FaceID })
		if index < 0 || !maps.Equal(faces[index].EntryIDs, mark.face.EntryIDs) || faces[index].ReregistrationRequired == mark.required {
			continue
		}
		faces[index].ReregistrationRequired = mark.required
		changed = append(changed, mark)
	}
	if len(changed) == 0 {
		return nil, nil
	}
	return changed, s.save()
}

// checkReferenceFaces는 서버 기록과 워커 화이트리스트를 한 번 대조해 재등록 필요
// 표시를 갱신한다.
//
// 답한 워커 중 하나라도 얼굴을 갖지 않으면 표시한다 — 그 워커에 배정된 세션에서는
// 등록한 사람도 블러된다. 모든 워커가 답했고 모두 가졌으면 표시를 지운다. 답하지
// 않은 워커가 있으면 그 얼굴의 표시를 바꾸지 않는다. 끊긴 워커 하나 때문에 모든
// 사용자에게 재등록을 요청하지도, 실제로 잃은 얼굴의 표시를 지우지도 않는다.
func (s *Server) checkReferenceFaces(ctx context.Context) {
	if s.ai == nil {
		return
	}
	snapshot := s.references.snapshot()
	if len(snapshot) == 0 {
		return
	}
	clientIDs := slices.Sorted(maps.Keys(snapshot))
	ctx, cancel := context.WithTimeout(ctx, referenceCheckTimeout)
	defer cancel()
	observed := s.ai.WhitelistEntryIDs(ctx, clientIDs)

	var marks []referenceMark
	for _, clientID := range clientIDs {
		for _, face := range snapshot[clientID] {
			verdict := judgeReferenceFace(face, clientID, observed)
			switch {
			case len(verdict.missing) > 0:
				marks = append(marks, referenceMark{clientID: clientID, face: face, required: true, missing: verdict.missing})
			case verdict.complete:
				marks = append(marks, referenceMark{clientID: clientID, face: face, required: false})
			}
		}
	}
	changed, err := s.references.applyMarks(marks)
	if err != nil {
		s.logger.Warn("save reference face re-registration marks failed", "error", err)
	}
	for _, mark := range changed {
		if mark.required {
			s.logger.Warn("reference face missing on AI workers; re-registration required",
				"client_id", mark.clientID, "face_id", mark.face.FaceID, "missing_targets", mark.missing)
			continue
		}
		s.logger.Info("reference face present on every AI worker; re-registration mark cleared",
			"client_id", mark.clientID, "face_id", mark.face.FaceID)
	}
}

// RunReferenceFaceCheck는 ctx가 끝날 때까지 서버 기록과 AI 워커 화이트리스트를
// 대조한다. 시작하자마자 한 번, 그 뒤로 referenceCheckInterval마다, 그리고
// RequestReferenceFaceCheck가 요청할 때마다 돈다. AI가 꺼져 있으면 바로 돌아온다.
func (s *Server) RunReferenceFaceCheck(ctx context.Context) {
	if s.ai == nil {
		return
	}
	ticker := time.NewTicker(referenceCheckInterval)
	defer ticker.Stop()
	for {
		s.checkReferenceFaces(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.referenceCheckRequests:
		}
	}
}

// RequestReferenceFaceCheck는 주기를 기다리지 않고 대조를 한 번 더 돌게 한다. AI
// 워커가 다시 준비 상태가 됐을 때(재시작 뒤) 부른다. 걸려 있는 요청과는 합친다.
func (s *Server) RequestReferenceFaceCheck() {
	select {
	case s.referenceCheckRequests <- struct{}{}:
	default:
	}
}
