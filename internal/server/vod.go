package server

import (
	"context"
	"net/http"
	"sync"

	"inno-live-server/internal/database"
	"inno-live-server/internal/vod"
)

// This lifecycle joins the existing server background context and shutdown hook
// without changing the deployment/session-handoff entrypoint from PR #519.
type vodRuntime struct {
	service *vod.Service
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool
}

func (s *Server) configureVOD() {
	if !s.cfg.VOD.Enabled {
		return
	}
	service := vod.New(s.cfg.VOD, nil, s.logger)
	s.vod = &vodRuntime{service: service}
	service.Register(s.mux, func(h http.Handler) http.Handler { return s.requireUser(s.withUserOperation(h)) })
}
func (s *Server) startVOD(ctx context.Context) func() {
	if !s.cfg.VOD.Enabled || s.vod == nil {
		return func() {}
	}
	runtime := s.vod
	runtime.mu.Lock()
	if runtime.stopped || runtime.done != nil {
		runtime.mu.Unlock()
		return func() {}
	}
	child, cancel := context.WithCancel(ctx)
	runtime.cancel = cancel
	runtime.done = make(chan struct{})
	done := runtime.done
	runtime.mu.Unlock()
	go func() {
		defer close(done)
		conn, err := database.Open(child, database.Options{URL: s.cfg.DatabaseURL, MaxOpenConns: s.cfg.DatabaseMaxOpenConns, MaxIdleConns: s.cfg.DatabaseMaxIdleConns, ConnMaxLifetime: s.cfg.DatabaseConnMaxLifetime, ConnMaxIdleTime: s.cfg.DatabaseConnMaxIdleTime})
		if err != nil {
			s.logger.Error("vod database unavailable")
			return
		}
		defer conn.Close()
		runtime.service.SetStore(vod.NewStore(conn.DB))
		defer runtime.service.SetStore(nil)
		var memory vod.MemoryProbe = vod.NvidiaMemory{}
		if s.cfg.VOD.StaticFreeMiB != nil {
			memory = vod.StaticMemory{Free: *s.cfg.VOD.StaticFreeMiB, GPUs: s.cfg.VOD.WorkerGPUs}
		}
		engine := vod.NewEngine(runtime.service, s.ai, vod.GRPCWorker{Pool: s.ai}, memory)
		engine.Run(child)
	}()
	return cancel
}
func (s *Server) stopVOD(ctx context.Context) error {
	if s.vod == nil {
		return nil
	}
	s.vod.mu.Lock()
	s.vod.stopped = true
	cancel, done := s.vod.cancel, s.vod.done
	s.vod.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
