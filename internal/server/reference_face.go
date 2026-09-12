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

	"inno-live-server/internal/auth"
	"inno-live-server/internal/session"

	"github.com/google/uuid"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const maxReferenceUpload = 10 << 20

// maxAIFaceEdge is the AI worker's B1-640 long-edge limit. Uploads larger than
// this are rejected by AddWhitelist, so we downscale before registering.
const maxAIFaceEdge = 640

// maxDecodeEdge/maxDecodePixels bound the dimensions we are willing to decode.
// DecodeConfig reads them from the header, so an oversized image is rejected
// before its full bitmap is allocated — a small but highly compressible file
// can otherwise decode into hundreds of MiB. The result is downscaled to
// maxAIFaceEdge, so a reference photo never needs to exceed these.
const (
	maxDecodeEdge   = 4096
	maxDecodePixels = 16 << 20 // 16,777,216 px (~16MP)
)

// errImageTooLarge is returned by downscaleForAI when an image's header reports
// dimensions past the decode limits.
var errImageTooLarge = errors.New("image exceeds decode limits")

// decodeGate bounds how many uploaded images are decoded at the same moment
// across the whole process. Each decode transiently allocates the full bitmap
// (bounded by maxDecodePixels), so unbounded concurrency lets simultaneous
// uploads stack that memory and starve the real-time media path. The token is
// held only across the decode, never across the AI worker call.
//
// A nil *decodeGate is valid and means unlimited.
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

// downscaleForAI shrinks an uploaded face image so its long edge is at most
// maxAIFaceEdge, re-encoding as JPEG. Images already within the limit (or that
// fail to decode here) are returned unchanged so the AI worker still applies its
// own validation and error reporting. Images whose header reports dimensions
// past maxDecodeEdge/maxDecodePixels are rejected with errImageTooLarge before
// the full bitmap is decoded.
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

// referenceFace is the stored form of one registered face. It is persisted to
// disk but never written to an API response — referenceFaceView is.
type referenceFace struct {
	FaceID string `json:"face_id"`
	// EntryIDs maps an AI worker address to the entry id that worker minted for
	// this face. Every worker generates its own id, so deleting the face means
	// addressing each worker with its own id.
	EntryIDs     map[string]string `json:"entry_ids,omitempty"`
	RegisteredAt time.Time         `json:"registered_at"`
}

// referenceFaceView is the public shape of a registered face: the worker entry
// ids are internal bookkeeping and stay out of the API.
type referenceFaceView struct {
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
	path          string // JSON persistence path; "" disables persistence
	envConfigured bool   // AI_PRIVACY_ME_IMAGE_PATH set → env default reference
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

// save persists the client→faces map. The caller must hold s.mu for writing.
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
	// Write and rename in the same directory. A direct WriteFile can truncate
	// the only copy before an I/O error, leaving metadata for every other user
	// unreadable. Rename publishes the complete snapshot in one filesystem
	// operation.
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
	r.Body = http.MaxBytesReader(w, r.Body, maxReferenceUpload*20)
	if err := r.ParseMultipartForm(maxReferenceUpload); err != nil {
		writeError(w, badRequest("Invalid multipart image upload.", nil))
		return
	}
	// ParseMultipartForm may spill files above its memory budget to disk. Remove
	// those request-scoped copies after every upload path returns.
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
	// "image" (single field) replaces the client's set; "images[]" appends.
	replace := len(r.MultipartForm.File["image"]) > 0 && len(r.MultipartForm.File["images"]) == 0
	if replace {
		// Clear the client's existing worker whitelist first so stale faces stop
		// being excluded (matches Python's replace semantics). A failure here has
		// to abort the upload: leaving the previous faces whitelisted keeps
		// excluding people the client just replaced.
		if err := s.ai.ClearWhitelist(r.Context(), clientID); err != nil {
			s.logger.Error("clear whitelist before replace failed", "client_id", clientID, "error", err)
			writeError(w, apiError{Status: http.StatusBadGateway, Code: "ai_unavailable", Message: "AI whitelist replacement failed."})
			return
		}
	}
	registered := make([]referenceFace, 0, len(files))
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
		faceID := uuid.NewString()
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
		result, err := s.ai.AddWhitelist(r.Context(), clientID, scaled)
		if err != nil {
			s.logger.Error("AI AddWhitelist failed", "client_id", clientID, "error", err)
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
			writeError(w, apiError{Status: http.StatusBadRequest, Code: code, Message: "AI 서버가 기준 얼굴 등록을 거부했습니다.", Details: map[string]any{"reason": msg}})
			return
		}
		registered = append(registered, referenceFace{FaceID: faceID, EntryIDs: result.EntryIDs, RegisteredAt: time.Now().UTC()})
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
		// A face persisted before per-worker entry ids were recorded cannot be
		// targeted individually. Clearing the whole session is the only way to
		// stop excluding it, so drop the client's other faces too rather than
		// report a registration the workers no longer hold.
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
		views = append(views, referenceFaceView{FaceID: face.FaceID, RegisteredAt: face.RegisteredAt})
	}
	result := referenceStatus{ClientID: clientID, Count: len(faces), Faces: views}
	if len(faces) > 0 {
		source := "api"
		registeredAt := faces[0].RegisteredAt
		result.Registered = true
		result.Source = &source
		result.RegisteredAt = &registeredAt
	} else if envConfigured {
		// No API faces for this client, but an env default reference is set.
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
	// Reference-face endpoints are mounted behind RequireUser in production.
	// A deterministic fallback keeps direct handler tests independent from the
	// authentication package without accepting a caller-controlled bucket.
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
