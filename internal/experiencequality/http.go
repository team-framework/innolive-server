package experiencequality

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"
)

const MaxBodyBytes = 1024
const IngestPerMinute = 1200

type EventStore interface {
	Save(context.Context, Event) error
}

type Handler struct {
	store   EventStore
	keyHash [32]byte
	mu      sync.Mutex
	window  time.Time
	count   int
}

func NewHandler(store EventStore, key string) *Handler {
	return &Handler{store: store, keyHash: sha256.Sum256([]byte("Bearer " + key))}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	digest := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	if subtle.ConstantTimeCompare(digest[:], h.keyHash[:]) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !h.allow(time.Now()) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	if r.ContentLength > MaxBodyBytes {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
		return
	}
	e, err := Decode(bytes.NewReader(body))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.store.Save(ctx, e); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) allow(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.window.IsZero() || now.Sub(h.window) >= time.Minute {
		h.window = now
		h.count = 0
	}
	if h.count >= IngestPerMinute {
		return false
	}
	h.count++
	return true
}
