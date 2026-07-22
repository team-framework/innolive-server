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

	"inno-live-server/internal/config"
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

	resolvedFFmpeg, lookupErr := exec.LookPath(cfg.FFmpegPath)
	if lookupErr != nil {
		logger.Error("FFmpeg is required in every comparison mode", "path", cfg.FFmpegPath, "error", lookupErr)
		os.Exit(2)
	}
	cfg.FFmpegPath = resolvedFFmpeg

	sessionManager, err := session.NewManager(cfg, logger, registry)
	if err != nil {
		logger.Error("create session manager failed", "error", err)
		os.Exit(1)
	}
	defer sessionManager.CloseAll()
	application := server.New(cfg, logger, registry, sessionManager)

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           application.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("Go comparison server started",
			"address", cfg.HTTPAddr,
			"privacy_mode", cfg.PrivacyMode,
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
