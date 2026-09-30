package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"inno-live-server/internal/ai"
	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"

	"github.com/google/uuid"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const maxReferenceUpload = 10 << 20

// referenceUploadReadTimeout은 요청 본문 전체가 도착하기까지 기다리는 한계다.
// 모바일 회선에서 수 MB를 올리기에 넉넉하고, 멈춘 송신자가 자리를 오래 붙잡지
// 않을 만큼 짧다.
var referenceUploadReadTimeout = 60 * time.Second

// maxAIFaceEdge는 AI 워커의 B1-640 긴 변 한계다. 이보다 크면 AddWhitelist가
// 거절하므로 등록 전에 줄인다.
const maxAIFaceEdge = 640

// maxDecodeEdge/maxDecodePixels는 디코드를 허용하는 크기 한계다. DecodeConfig가
// 헤더에서 읽으므로 큰 이미지는 전체 비트맵을 할당하기 전에 거절된다 — 작지만
// 압축률이 높은 파일은 그러지 않으면 수백 MiB로 풀릴 수 있다. 결과는
// maxAIFaceEdge로 줄이므로 기준 사진이 이 한계를 넘을 필요는 없다.
const (
	maxDecodeEdge   = 4096
	maxDecodePixels = 16 << 20 // 16,777,216 px (~16MP)
)

// errImageTooLarge는 헤더가 알린 크기가 디코드 한계를 넘을 때 downscaleForAI가
// 돌려준다.
var errImageTooLarge = errors.New("image exceeds decode limits")

// decodeGate는 프로세스 전체에서 동시에 디코드하는 업로드 이미지 수를 제한한다.
// 디코드마다 전체 비트맵(maxDecodePixels 이하)을 잠시 할당하므로, 제한이 없으면
// 동시 업로드가 그 메모리를 쌓아 실시간 미디어 경로를 굶긴다. 토큰은 디코드
// 동안만 쥐고 AI 워커 호출 동안에는 쥐지 않는다.
//
// nil *decodeGate는 유효하며 제한 없음을 뜻한다.
type decodeGate struct {
	tokens chan struct{}
}

func newDecodeGate(size int) *decodeGate {
	if size <= 0 {
		return nil
	}
	return &decodeGate{tokens: make(chan struct{}, size)}
}

func (g *decodeGate) acquire(ctx context.Context) error {
	if g == nil {
		return nil
	}
	select {
	case g.tokens <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *decodeGate) release() {
	if g == nil {
		return
	}
	<-g.tokens
}

// downscaleForAI는 업로드한 얼굴 이미지의 긴 변이 maxAIFaceEdge 이하가 되도록
// 줄여 JPEG로 다시 인코딩한다. 이미 한계 안이거나 여기서 디코드하지 못한
// 이미지는 그대로 돌려줘 AI 워커가 자체 검증과 오류 보고를 하게 한다. 헤더가
// 알린 크기가 maxDecodeEdge/maxDecodePixels를 넘으면 전체 비트맵을 디코드하기
// 전에 errImageTooLarge로 거절한다.
func downscaleForAI(data []byte) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return data, nil
	}
	if cfg.Width > maxDecodeEdge || cfg.Height > maxDecodeEdge || cfg.Width*cfg.Height > maxDecodePixels {
		return nil, errImageTooLarge
	}
	if cfg.Width <= maxAIFaceEdge && cfg.Height <= maxAIFaceEdge {
		return data, nil
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, nil
	}
	b := src.Bounds()
	scale := float64(maxAIFaceEdge) / float64(max(b.Dx(), b.Dy()))
	dst := image.NewRGBA(image.Rect(0, 0, int(float64(b.Dx())*scale), int(float64(b.Dy())*scale)))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 90}); err != nil {
		return data, nil
	}
	return out.Bytes(), nil
}

// referenceFace는 등록한 얼굴 하나의 저장 형태다. 디스크에는 남지만 API 응답에는
// 쓰지 않는다 — 응답은 referenceFaceView다.
type referenceFace struct {
	Name   string `json:"name,omitempty"`
	FaceID string `json:"face_id"`
	// EntryIDs는 AI 워커 주소별로 그 워커가 이 얼굴에 발급한 엔트리 id다. 워커마다
	// id를 따로 만들므로, 얼굴을 지우려면 각 워커에 그 워커의 id로 요청해야 한다.
	EntryIDs     map[string]string `json:"entry_ids,omitempty"`
	RegisteredAt time.Time         `json:"registered_at"`
}

// referenceFaceView는 등록한 얼굴의 공개 형태다. 워커 엔트리 id는 내부 기록이라
// API에 내보내지 않는다.
type referenceFaceView struct {
	Name         string    `json:"name,omitempty"`
	FaceID       string    `json:"face_id"`
	RegisteredAt time.Time `json:"registered_at"`
}

type referenceStatus struct {
	Registered   bool                `json:"registered"`
	Source       *string             `json:"source"`
	RegisteredAt *time.Time          `json:"registered_at"`
	ClientID     string              `json:"client_id"`
	Count        int                 `json:"count"`
	Faces        []referenceFaceView `json:"faces"`
}

type referenceStore struct {
	mu            sync.RWMutex
	faces         map[string][]referenceFace
	path          string // JSON 저장 경로. ""이면 저장하지 않는다
	envConfigured bool   // AI_PRIVACY_ME_IMAGE_PATH가 있으면 env 기본 기준 얼굴을 쓴다
}

type guestReferenceContextKey struct{}

type guestReferenceGateContextKey struct{}

// guestReferenceGate는 하나의 guest 세션에 대한 얼굴 변경과 종료 정리를 직렬화한다.
// 세션 종료는 즉시 terminal 상태를 표시하고, 정리는 진행 중인 upload가 끝난 뒤 worker
// whitelist와 저장된 메타데이터를 비운다.
type guestReferenceGate struct {
	mu      sync.Mutex
	entries map[string]*guestReferenceGateEntry
}

type guestReferenceGateEntry struct {
	mu     sync.Mutex
	refs   int
	closed bool
}

func newGuestReferenceGate() *guestReferenceGate {
	return &guestReferenceGate{entries: make(map[string]*guestReferenceGateEntry)}
}

func (g *guestReferenceGate) Lock(sessionID string) (unlock func(), closed bool) {
	g.mu.Lock()
	entry := g.entries[sessionID]
	if entry == nil {
		entry = &guestReferenceGateEntry{}
		g.entries[sessionID] = entry
	}
	entry.refs++
	closed = entry.closed
	g.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		g.release(sessionID, entry)
	}, closed
}

// Close는 진행 중인 작업을 기다리지 않고 세션을 terminal 상태로 표시한다.
// 반환된 release 함수는 terminal 정리가 끝난 뒤에만 실행해야 한다.
func (g *guestReferenceGate) Close(sessionID string) func() {
	g.mu.Lock()
	entry := g.entries[sessionID]
	if entry == nil {
		entry = &guestReferenceGateEntry{}
		g.entries[sessionID] = entry
	}
	entry.refs++
	entry.closed = true
	g.mu.Unlock()
	return func() { g.release(sessionID, entry) }
}

func (g *guestReferenceGate) IsClosed(sessionID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.entries[sessionID]
	return entry != nil && entry.closed
}

func (g *guestReferenceGate) release(sessionID string, entry *guestReferenceGateEntry) {
	g.mu.Lock()
	entry.refs--
	if entry.refs == 0 && g.entries[sessionID] == entry {
		delete(g.entries, sessionID)
	}
	g.mu.Unlock()
}

func newReferenceStore(path string, envConfigured bool) *referenceStore {
	s := &referenceStore{faces: make(map[string][]referenceFace), path: path, envConfigured: envConfigured}
	s.load()
	return s
}

func (s *referenceStore) load() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var faces map[string][]referenceFace
	if json.Unmarshal(data, &faces) == nil && faces != nil {
		s.faces = faces
	}
}

// save는 클라이언트→얼굴 맵을 저장한다. 호출자가 s.mu를 쓰기로 쥐고 있어야 한다.
func (s *referenceStore) save() error {
	if s.path == "" {
		return nil
	}
	data, err := json.Marshal(s.faces)
	if err != nil {
		return fmt.Errorf("marshal reference metadata: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create reference metadata directory: %w", err)
	}
	// 같은 디렉터리에 쓰고 이름을 바꾼다. WriteFile로 바로 쓰면 I/O 오류 전에 유일한
	// 사본을 잘라 다른 사용자의 메타데이터까지 읽을 수 없게 될 수 있다. rename은
	// 완성된 스냅샷을 파일시스템 연산 한 번으로 공개한다.
	temporary, err := os.CreateTemp(dir, ".reference-faces-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary reference metadata: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set reference metadata permissions: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary reference metadata: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary reference metadata: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary reference metadata: %w", err)
	}
	if err := os.Rename(temporaryName, s.path); err != nil {
		return fmt.Errorf("publish reference metadata: %w", err)
	}
	return nil
}

func (s *Server) handlePostReferenceFace(w http.ResponseWriter, r *http.Request) {
	if s.ai == nil {
		writeError(w, apiError{Status: http.StatusBadRequest, Code: "bad_request", Message: "AI privacy mode is not enabled.", Details: map[string]any{"reason": "ai_disabled"}})
		return
	}
	// 서버에는 본문 읽기 타임아웃이 없다(전역으로 걸면 signaling 웹소켓과 게스트 SSE
	// 스트림까지 끊긴다). 그래서 이 업로드만 여기서 제한한다. 본문은
	// maxReferenceUpload*20까지 가능하고, 그 바이트를 조금씩 흘리는 연결은 제한이
	// 없으면 핸들러를 무한정 붙잡는다(#207).
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(referenceUploadReadTimeout)); err != nil {
		s.logger.Warn("set reference upload read deadline failed", "error", err)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxReferenceUpload*20)
	if err := r.ParseMultipartForm(maxReferenceUpload); err != nil {
		writeError(w, badRequest("Invalid multipart image upload.", nil))
		return
	}
	// ParseMultipartForm은 메모리 예산을 넘는 파일을 디스크에 쏟을 수 있다. 업로드
	// 경로가 어떻게 끝나든 요청 범위의 그 사본을 지운다.
	if r.MultipartForm != nil {
		defer func() {
			if err := r.MultipartForm.RemoveAll(); err != nil && s.logger != nil {
				s.logger.Warn("remove temporary reference upload failed", "error", err)
			}
		}()
	}
	clientID := referenceClientID(r)
	files := append([]*multipart.FileHeader(nil), r.MultipartForm.File["image"]...)
	files = append(files, r.MultipartForm.File["images"]...)
	if len(files) == 0 {
		writeError(w, badRequest("업로드할 이미지가 없습니다.", map[string]any{"reason": "empty_upload"}))
		return
	}
	if len(files) > 20 {
		writeError(w, badRequest("이미지는 최대 20개까지 업로드할 수 있습니다.", nil))
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 40 || strings.ContainsAny(name, "\r\n\x00") || (name != "" && len(files) != 1) {
		writeError(w, badRequest("name must be at most 40 characters and apply to one image.", map[string]any{"field": "name"}))
		return
	}
	// "image"(단일 필드)는 클라이언트의 얼굴 집합을 교체하고, "images[]"는 추가한다.
	replace := len(r.MultipartForm.File["image"]) > 0 && len(r.MultipartForm.File["images"]) == 0
	if replace {
		// 클라이언트의 기존 워커 화이트리스트를 먼저 비워 오래된 얼굴이 더는 제외되지
		// 않게 한다(Python의 교체 동작과 같다). 여기서 실패하면 업로드를 중단해야
		// 한다 — 이전 얼굴을 화이트리스트에 남기면 방금 교체한 사람을 계속 제외한다.
		if err := s.ai.ClearWhitelist(r.Context(), clientID); err != nil {
			s.logger.Error("clear whitelist before replace failed", "client_id", clientID, "error", err)
			writeError(w, apiError{Status: http.StatusBadGateway, Code: "ai_unavailable", Message: "AI whitelist replacement failed."})
			return
		}
	}
	// AI 워커를 건드리기 전에 모든 파일을 검증·디코드한다. 잘못된 파일이 있으면
	// 아무것도 등록하지 않고 요청을 끝내므로, 뒤 파일의 형식·크기 오류가 앞 파일의
	// 얼굴을 워커에 남기는 일이 없다.
	type preparedFace struct {
		faceID string
		data   []byte
	}
	prepared := make([]preparedFace, 0, len(files))
	for _, header := range files {
		contentType := header.Header.Get("Content-Type")
		if contentType != "image/jpeg" && contentType != "image/png" && contentType != "image/webp" {
			writeError(w, badRequest("지원하지 않는 이미지 형식입니다.", nil))
			return
		}
		file, err := header.Open()
		if err != nil {
			writeError(w, badRequest("이미지를 읽을 수 없습니다.", nil))
			return
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxReferenceUpload+1))
		_ = file.Close()
		if readErr != nil || len(data) == 0 || len(data) > maxReferenceUpload {
			writeError(w, badRequest("유효하지 않은 이미지입니다.", map[string]any{"reason": "empty_or_oversized_file"}))
			return
		}
		if err := s.referenceDecode.acquire(r.Context()); err != nil {
			writeError(w, apiError{Status: http.StatusServiceUnavailable, Code: "server_busy", Message: "이미지 처리 대기 중 요청이 취소되었습니다."})
			return
		}
		scaled, err := downscaleForAI(data)
		s.referenceDecode.release()
		if err != nil {
			writeError(w, badRequest("이미지 크기가 너무 큽니다.", map[string]any{"reason": "image_too_large", "max_edge": maxDecodeEdge, "max_pixels": maxDecodePixels}))
			return
		}
		prepared = append(prepared, preparedFace{faceID: uuid.NewString(), data: scaled})
	}

	// 준비한 얼굴을 워커에 하나씩 등록한다. 하나라도 실패하면 이 요청에서 이미 등록한
	// 엔트리를 되돌려, API가 미등록이라고 알린 얼굴을 워커가 쥐고 있지 않게 한다.
	// 이전 요청이 등록한 얼굴(추가 모드)은 건드리지 않는다.
	registered := make([]referenceFace, 0, len(prepared))
	for _, face := range prepared {
		result, err := s.ai.AddWhitelist(r.Context(), clientID, face.data)
		if err != nil {
			s.logger.Error("AI AddWhitelist failed", "client_id", clientID, "error", err)
			s.rollbackReferenceRegistrations(r.Context(), clientID, registered)
			writeError(w, apiError{Status: http.StatusBadGateway, Code: "ai_unavailable", Message: "AI whitelist registration failed."})
			return
		}
		if strings.HasPrefix(result.Response.GetStatusMessage(), "failed") {
			msg := result.Response.GetStatusMessage()
			code := "reference_rejected"
			switch {
			case strings.Contains(msg, "No face"), strings.Contains(msg, "landmark"):
				code = "face_not_detected"
			case strings.Contains(msg, "read"), strings.Contains(msg, "decode"), strings.Contains(msg, "image"):
				code = "invalid_image"
			}
			s.rollbackReferenceRegistrations(r.Context(), clientID, registered)
			writeError(w, apiError{Status: http.StatusBadRequest, Code: code, Message: "AI 서버가 기준 얼굴 등록을 거부했습니다.", Details: map[string]any{"reason": msg}})
			return
		}
		registered = append(registered, referenceFace{Name: name, FaceID: face.faceID, EntryIDs: result.EntryIDs, RegisteredAt: time.Now().UTC()})
	}
	if gate, sessionID, ok := guestReferenceGateFromContext(r.Context()); ok && gate.IsClosed(sessionID) {
		// 이 요청이 얼굴을 등록하는 동안 세션이 종료됐다. 이미 terminal 정리가 수행한
		// 결과를 다시 저장하지 않는다.
		if err := s.ai.ClearWhitelist(r.Context(), clientID); err != nil {
			s.logger.Error("clear whitelist after guest session close failed", "client_id", clientID, "error", err)
			writeError(w, apiError{Status: http.StatusBadGateway, Code: "ai_unavailable", Message: "Guest session ended while registering a reference face."})
			return
		}
		s.references.deleteClient(clientID)
		writeError(w, apiError{Status: http.StatusConflict, Code: "session_ended", Message: "Guest session ended while registering a reference face."})
		return
	}

	s.references.mu.Lock()
	if replace {
		s.references.faces[clientID] = registered
	} else {
		s.references.faces[clientID] = append(s.references.faces[clientID], registered...)
	}
	_ = s.references.save()
	s.references.mu.Unlock()
	writeJSON(w, http.StatusCreated, s.references.status(clientID))
}

// referenceRollbackTimeout은 분리된 롤백의 한계다. 응답이 이미 정해진 뒤 멈춘
// 워커가 핸들러를 붙잡지 못하게 한다.
const referenceRollbackTimeout = 5 * time.Second

// rollbackReferenceRegistrations는 이 요청에서 지금까지 등록한 얼굴의 워커
// 화이트리스트 엔트리를 워커마다 그 워커가 발급한 id로 지운다. 최선 노력이다 —
// 호출자가 이미 등록 실패를 클라이언트에 알리고 있으므로 삭제 실패는 로그만 남긴다.
//
// 요청 컨텍스트를 먼저 분리한다. 업로드 도중 클라이언트가 끊으면 컨텍스트가
// 취소되는데, 그것이 AddWhitelist가 실패하는 경로 중 하나다 — 취소된 컨텍스트로
// 롤백하면 곧바로 실패해 이 롤백이 지우려는 바로 그 엔트리가 남는다(#201).
func (s *Server) rollbackReferenceRegistrations(ctx context.Context, clientID string, registered []referenceFace) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), referenceRollbackTimeout)
	defer cancel()
	for _, face := range registered {
		err := ai.RetryWhitelistDelete(ctx, func(c context.Context) error {
			return s.ai.DeleteWhitelistEntries(c, clientID, face.EntryIDs)
		})
		if err != nil {
			s.logger.Error("rollback reference whitelist entries failed after retries; stale worker entries may remain",
				"client_id", clientID, "face_id", face.FaceID, "error", err)
		}
	}
}

func (s *Server) handleGetReferenceFace(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.references.status(referenceClientID(r)))
}

func (s *Server) handleDeleteReferenceFace(w http.ResponseWriter, r *http.Request) {
	if s.ai == nil {
		writeError(w, apiError{Status: http.StatusBadRequest, Code: "bad_request", Message: "AI privacy mode is not enabled.", Details: map[string]any{"reason": "ai_disabled"}})
		return
	}
	clientID := referenceClientID(r)
	if err := s.ai.ClearWhitelist(r.Context(), clientID); err != nil {
		s.logger.Error("AI ClearWhitelist failed", "client_id", clientID, "error", err)
		writeError(w, apiError{Status: http.StatusBadGateway, Code: "ai_unavailable", Message: "AI whitelist deletion failed."})
		return
	}
	s.references.mu.Lock()
	delete(s.references.faces, clientID)
	_ = s.references.save()
	s.references.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteReferenceFaceByID(w http.ResponseWriter, r *http.Request) {
	if s.ai == nil {
		writeError(w, apiError{Status: http.StatusBadRequest, Code: "bad_request", Message: "AI privacy mode is not enabled.", Details: map[string]any{"reason": "ai_disabled"}})
		return
	}
	clientID := referenceClientID(r)
	faceID := r.PathValue("face_id")
	s.references.mu.RLock()
	found := false
	var entryIDs map[string]string
	for _, f := range s.references.faces[clientID] {
		if f.FaceID == faceID {
			found = true
			entryIDs = f.EntryIDs
			break
		}
	}
	s.references.mu.RUnlock()
	if !found {
		writeError(w, apiError{Status: http.StatusNotFound, Code: "not_found", Message: "기준 얼굴을 찾을 수 없습니다.", Details: map[string]any{"face_id": faceID}})
		return
	}
	if len(entryIDs) == 0 {
		// 워커별 엔트리 id를 기록하기 전에 저장된 얼굴은 하나만 골라 지울 수 없다.
		// 제외를 멈추는 유일한 방법이 세션 전체를 비우는 것이라, 워커가 더는 쥐고
		// 있지 않은 등록을 알리느니 클라이언트의 다른 얼굴도 함께 지운다.
		s.logger.Warn("reference face has no worker entry ids; clearing the whole client whitelist",
			"client_id", clientID, "face_id", faceID)
		s.handleDeleteReferenceFace(w, r)
		return
	}
	if err := s.ai.DeleteWhitelistEntries(r.Context(), clientID, entryIDs); err != nil {
		s.logger.Error("AI DeleteWhitelistEntries failed", "client_id", clientID, "face_id", faceID, "error", err)
		writeError(w, apiError{Status: http.StatusBadGateway, Code: "ai_unavailable", Message: "AI whitelist deletion failed."})
		return
	}
	s.references.mu.Lock()
	remaining := s.references.faces[clientID][:0]
	for _, f := range s.references.faces[clientID] {
		if f.FaceID != faceID {
			remaining = append(remaining, f)
		}
	}
	if len(remaining) == 0 {
		delete(s.references.faces, clientID)
	} else {
		s.references.faces[clientID] = remaining
	}
	_ = s.references.save()
	s.references.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *referenceStore) status(clientID string) referenceStatus {
	s.mu.RLock()
	faces := append([]referenceFace(nil), s.faces[clientID]...)
	envConfigured := s.envConfigured
	s.mu.RUnlock()
	views := make([]referenceFaceView, 0, len(faces))
	for _, face := range faces {
		views = append(views, referenceFaceView{Name: face.Name, FaceID: face.FaceID, RegisteredAt: face.RegisteredAt})
	}
	result := referenceStatus{ClientID: clientID, Count: len(faces), Faces: views}
	if len(faces) > 0 {
		source := "api"
		registeredAt := faces[0].RegisteredAt
		result.Registered = true
		result.Source = &source
		result.RegisteredAt = &registeredAt
	} else if envConfigured {
		// 이 클라이언트의 API 얼굴은 없지만 env 기본 기준 얼굴이 설정돼 있다.
		source := "env"
		result.Registered = true
		result.Source = &source
		result.Count = 1
	}
	return result
}

func referenceClientID(r *http.Request) string {
	if guestID, ok := r.Context().Value(guestReferenceContextKey{}).(string); ok && guestID != "" {
		return guestID
	}
	if userID, ok := auth.UserIDFromContext(r.Context()); ok {
		return session.AIClientIDForUser(userID)
	}
	// 기준 얼굴 엔드포인트는 프로덕션에서 RequireUser 뒤에 달린다. 결정적인 대체
	// 값은 핸들러를 직접 부르는 테스트를 인증 패키지와 떼어 두면서도 호출자가 고른
	// 버킷을 받지 않게 한다.
	return "default"
}

func guestReferenceGateFromContext(ctx context.Context) (*guestReferenceGate, string, bool) {
	value, ok := ctx.Value(guestReferenceGateContextKey{}).(struct {
		gate      *guestReferenceGate
		sessionID string
	})
	if !ok || value.gate == nil || value.sessionID == "" {
		return nil, "", false
	}
	return value.gate, value.sessionID, true
}

func (s *referenceStore) deleteClient(clientID string) error {
	s.mu.Lock()
	previous, hadPrevious := s.faces[clientID]
	delete(s.faces, clientID)
	err := s.save()
	if err != nil && hadPrevious {
		s.faces[clientID] = previous
	}
	s.mu.Unlock()
	return err
}
