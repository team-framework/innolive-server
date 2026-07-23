package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"inno-live-server/internal/ai"
	"inno-live-server/internal/config"
	"inno-live-server/internal/media"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/server"
	"inno-live-server/internal/session"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	logger := newLogger(cfg.LogLevel)
	registry := metrics.New()

	if !cfg.RequireSessionAuth {
		logger.Warn("session ownership auth is DISABLED (INNOLIVE_REQUIRE_SESSION_AUTH=false); any client can control or hijack any session — for local development only")
	}

	resolvedFFmpeg, lookupErr := exec.LookPath(cfg.FFmpegPath)
	if lookupErr != nil {
		logger.Error("FFmpeg is required in every privacy mode", "path", cfg.FFmpegPath, "error", lookupErr)
		os.Exit(2)
	}
	cfg.FFmpegPath = resolvedFFmpeg

	var aiPool *ai.Pool
	if cfg.PrivacyMode == config.PrivacyModeReal {
		if !cfg.AIInsecure {
			logger.Error("AI_GRPC_INSECURE=false is not supported until the AI server exposes TLS")
			os.Exit(2)
		}
		aiPool, err = ai.NewPool(cfg.AITargets, cfg.AITimeout)
		if err != nil {
			logger.Error("create AI client pool failed", "error", err)
			os.Exit(1)
		}
		defer aiPool.Close()

		if err := aiPool.Preflight(context.Background(), string(cfg.AIWireFormat), cfg.AIPreflightTimeout); err != nil {
			if cfg.AIPreflightRequired {
				logger.Error("AI preflight failed; refusing to start (AI_PREFLIGHT_REQUIRED=true)",
					"error", err, "wire_format", cfg.AIWireFormat, "targets", cfg.AITargets)
				os.Exit(2)
			}
			logger.Warn("AI preflight failed; starting anyway (AI_PREFLIGHT_REQUIRED=false)",
				"error", err, "wire_format", cfg.AIWireFormat, "targets", cfg.AITargets)
		} else {
			logger.Info("AI preflight passed", "wire_format", cfg.AIWireFormat, "targets", cfg.AITargets)
		}
	}

	spawnGate := media.NewSpawnGate(cfg.FFmpegSpawnConcurrency)
	sessionManager, err := session.NewManager(cfg, logger, registry, aiPool, spawnGate)
	if err != nil {
		logger.Error("create session manager failed", "error", err)
		os.Exit(1)
	}
	defer sessionManager.CloseAll()
	application := server.New(cfg, logger, registry, sessionManager, aiPool)

	// Preload the env-provided default reference face (AI_PRIVACY_ME_IMAGE_PATH)
	// under the global bucket, so clients with no API-registered face fall back
	// to it (matches the Python server's env reference). Done async so a slow
	// first-registration JIT never blocks startup.
	if aiPool != nil && cfg.AIMeImagePath != "" {
		go func() {
			data, err := os.ReadFile(cfg.AIMeImagePath)
			if err != nil {
				logger.Warn("env reference face not readable", "path", cfg.AIMeImagePath, "error", err)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 130*time.Second)
			defer cancel()
			if _, err := aiPool.AddWhitelist(ctx, "", "env", data); err != nil {
				logger.Warn("env reference face registration failed", "path", cfg.AIMeImagePath, "error", err)
				return
			}
			logger.Info("env reference face registered", "path", cfg.AIMeImagePath)
		}()
	}

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           application.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("InnoLive media server started",
			"address", cfg.HTTPAddr,
			"privacy_mode", cfg.PrivacyMode,
			"ai_targets", cfg.AITargets,
			"wire_format", cfg.AIWireFormat,
			"max_sessions", cfg.MaxSessions,
			"ffmpeg_spawn_concurrency", cfg.FFmpegSpawnConcurrency,
		)
		serverErrors <- httpServer.ListenAndServe()
	}()

	signalContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-signalContext.Done():
		logger.Info("shutdown signal received")
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownContext); err != nil {
		logger.Error("graceful HTTP shutdown failed", "error", err)
	}
}

func newLogger(level string) *slog.Logger {
	logLevel := slog.LevelInfo
	switch level {
	case "DEBUG":
		logLevel = slog.LevelDebug
	case "WARN":
		logLevel = slog.LevelWarn
	case "ERROR":
		logLevel = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
}
