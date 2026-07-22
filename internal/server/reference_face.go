package server

import (
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const maxReferenceUpload = 10 << 20

type referenceFace struct {
	FaceID       string    `json:"face_id"`
	RegisteredAt time.Time `json:"registered_at"`
}

type referenceStatus struct {
	Registered   bool            `json:"registered"`
	Source       *string         `json:"source"`
	RegisteredAt *time.Time      `json:"registered_at"`
	ClientID     string          `json:"client_id"`
	Count        int             `json:"count"`
	Faces        []referenceFace `json:"faces"`
}

type referenceStore struct {
	mu            sync.RWMutex
	faces         map[string][]referenceFace
	path          string // JSON persistence path; "" disables persistence
	envConfigured bool   // AI_PRIVACY_ME_IMAGE_PATH set → env default reference
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
func (s *referenceStore) save() {
	if s.path == "" {
		return
	}
	data, err := json.Marshal(s.faces)
	if err != nil {
		return
	}
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	_ = os.WriteFile(s.path, data, 0o644)
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
		// being excluded (matches Python's replace semantics).
		if _, err := s.ai.RemoveWhitelist(r.Context(), clientID, ""); err != nil {
			s.logger.Warn("clear whitelist before replace failed", "client_id", clientID, "error", err)
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
		response, err := s.ai.AddWhitelist(r.Context(), clientID, faceID, data)
		if err != nil {
			s.logger.Error("AI AddWhitelist failed", "client_id", clientID, "error", err)
			writeError(w, apiError{Status: http.StatusBadGateway, Code: "ai_unavailable", Message: "AI whitelist registration failed."})
			return
		}
		if strings.HasPrefix(response.GetStatusMessage(), "failed") {
			msg := response.GetStatusMessage()
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
		registered = append(registered, referenceFace{FaceID: faceID, RegisteredAt: time.Now().UTC()})
	}

	s.references.mu.Lock()
	if replace {
		s.references.faces[clientID] = registered
	} else {
		s.references.faces[clientID] = append(s.references.faces[clientID], registered...)
	}
	s.references.save()
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
	if _, err := s.ai.RemoveWhitelist(r.Context(), clientID, ""); err != nil {
		s.logger.Error("AI RemoveWhitelist(all) failed", "client_id", clientID, "error", err)
		writeError(w, apiError{Status: http.StatusBadGateway, Code: "ai_unavailable", Message: "AI whitelist deletion failed."})
		return
	}
	s.references.mu.Lock()
	delete(s.references.faces, clientID)
	s.references.save()
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
	for _, f := range s.references.faces[clientID] {
		if f.FaceID == faceID {
			found = true
			break
		}
	}
	s.references.mu.RUnlock()
	if !found {
		writeError(w, apiError{Status: http.StatusNotFound, Code: "not_found", Message: "기준 얼굴을 찾을 수 없습니다.", Details: map[string]any{"face_id": faceID}})
		return
	}
	if _, err := s.ai.RemoveWhitelist(r.Context(), clientID, faceID); err != nil {
		s.logger.Error("AI RemoveWhitelist failed", "client_id", clientID, "face_id", faceID, "error", err)
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
	s.references.save()
	s.references.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *referenceStore) status(clientID string) referenceStatus {
	s.mu.RLock()
	faces := append([]referenceFace(nil), s.faces[clientID]...)
	envConfigured := s.envConfigured
	s.mu.RUnlock()
	result := referenceStatus{ClientID: clientID, Count: len(faces), Faces: faces}
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
	for _, value := range []string{r.URL.Query().Get("client_id"), r.FormValue("client_id"), r.Header.Get("X-Client-ID")} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "default"
}
