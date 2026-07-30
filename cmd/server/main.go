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

	"github.com/google/uuid"

	"inno-live-server/internal/ai"
	"inno-live-server/internal/auth"
	"inno-live-server/internal/config"
	"inno-live-server/internal/database"
	"inno-live-server/internal/database/migration"
	"inno-live-server/internal/media"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/origin"
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

	// PostgreSQL 연결과 마이그레이션에 사용할 시작 컨텍스트다.
	startupContext, startupCancel := context.WithTimeout(
		context.Background(),
		30*time.Second,
	)

	databaseConnection, err := database.Open(
		startupContext,
		database.Options{
			URL:             cfg.DatabaseURL,
			MaxOpenConns:    cfg.DatabaseMaxOpenConns,
			MaxIdleConns:    cfg.DatabaseMaxIdleConns,
			ConnMaxLifetime: cfg.DatabaseConnMaxLifetime,
			ConnMaxIdleTime: cfg.DatabaseConnMaxIdleTime,
			Debug:           cfg.LogLevel == "DEBUG",
		},
	)
	if err != nil {
		startupCancel()
		logger.Error("connect PostgreSQL failed", "error", err)
		os.Exit(1)
	}

	defer func() {
		if err := databaseConnection.Close(); err != nil {
			logger.Error("close PostgreSQL failed", "error", err)
		}
	}()

	// DATABASE_MIGRATION_MODE에 따라 auto, versioned, off 중 하나를 실행한다.
	if err := migration.Run(
		startupContext,
		databaseConnection.DB,
		cfg.DatabaseURL,
		cfg.DatabaseMigrationMode,
	); err != nil {
		startupCancel()
		logger.Error(
			"database migration failed",
			"mode", cfg.DatabaseMigrationMode,
			"error", err,
		)
		os.Exit(1)
	}

	startupCancel()

	logger.Info(
		"PostgreSQL ready",
		"migration_mode", cfg.DatabaseMigrationMode,
		"max_open_connections", cfg.DatabaseMaxOpenConns,
		"max_idle_connections", cfg.DatabaseMaxIdleConns,
	)

	if !cfg.RequireSessionAuth {
		logger.Warn(
			"session ownership auth is DISABLED " +
				"(INNOLIVE_REQUIRE_SESSION_AUTH=false); " +
				"any client can control or hijack any session — " +
				"for local development only",
		)
	}

	resolvedFFmpeg, lookupErr := exec.LookPath(cfg.FFmpegPath)
	if lookupErr != nil {
		logger.Error(
			"FFmpeg is required in every privacy mode",
			"path", cfg.FFmpegPath,
			"error", lookupErr,
		)
		os.Exit(2)
	}
	cfg.FFmpegPath = resolvedFFmpeg

	var aiPool *ai.Pool

	if cfg.PrivacyMode == config.PrivacyModeReal {
		if !cfg.AIInsecure {
			logger.Error(
				"AI_GRPC_INSECURE=false is not supported " +
					"until the AI server exposes TLS",
			)
			os.Exit(2)
		}

		aiPool, err = ai.NewPool(
			cfg.AITargets,
			cfg.AITimeout,
		)
		if err != nil {
			logger.Error(
				"create AI client pool failed",
				"error", err,
			)
			os.Exit(1)
		}
		defer aiPool.Close()

		err = aiPool.Preflight(
			context.Background(),
			string(cfg.AIWireFormat),
			cfg.AIPreflightTimeout,
		)
		if err != nil {
			if cfg.AIPreflightRequired {
				logger.Error(
					"AI preflight failed; refusing to start "+
						"(AI_PREFLIGHT_REQUIRED=true)",
					"error", err,
					"wire_format", cfg.AIWireFormat,
					"targets", cfg.AITargets,
				)
				os.Exit(2)
			}

			logger.Warn(
				"AI preflight failed; starting anyway "+
					"(AI_PREFLIGHT_REQUIRED=false)",
				"error", err,
				"wire_format", cfg.AIWireFormat,
				"targets", cfg.AITargets,
			)
		} else {
			logger.Info(
				"AI preflight passed",
				"wire_format", cfg.AIWireFormat,
				"targets", cfg.AITargets,
			)
		}
	}

	spawnGate := media.NewSpawnGate(
		cfg.FFmpegSpawnConcurrency,
	)

	sessionManager, err := session.NewManager(
		cfg,
		logger,
		registry,
		aiPool,
		spawnGate,
	)
	if err != nil {
		logger.Error(
			"create session manager failed",
			"error", err,
		)
		os.Exit(1)
	}
	defer sessionManager.CloseAll()

	tokenConfig, err := auth.LoadTokenConfigFromEnv()
	if err != nil {
		logger.Error("invalid token configuration", "error", err)
		os.Exit(2)
	}
	originConfig, err := origin.LoadFromEnv()
	if err != nil {
		logger.Error("invalid token HTTP configuration", "error", err)
		os.Exit(2)
	}
	tokenService := auth.NewTokenService(databaseConnection.DB, tokenConfig)
	googleOAuthConfig, err := auth.LoadGoogleOAuthConfigFromEnv()
	if err != nil {
		logger.Error("invalid Google OAuth configuration", "error", err)
		os.Exit(2)
	}
	var googleLogin *auth.GoogleLoginService
	if googleOAuthConfig.Enabled() {
		googleVerifier, err := auth.NewGoogleIDTokenVerifier(context.Background(), googleOAuthConfig)
		if err != nil {
			logger.Error("create Google ID token verifier failed", "error", err)
			os.Exit(2)
		}
		googleLogin, err = auth.NewGoogleLoginService(
			googleVerifier,
			auth.NewGormGoogleAccountResolver(databaseConnection.DB),
			tokenService,
		)
		if err != nil {
			logger.Error("create Google login service failed", "error", err)
			os.Exit(2)
		}
	}
	appleOAuthConfig, err := auth.LoadAppleOAuthConfigFromEnv()
	if err != nil {
		logger.Error("invalid Apple OAuth configuration", "error", err)
		os.Exit(2)
	}
	var appleLogin *auth.AppleLoginService
	var appleRevoker auth.AppleTokenRevoker
	var providerTokenCipher *auth.ProviderTokenCipher
	if appleOAuthConfig.Enabled() {
		providerTokenCipher, err = auth.NewProviderTokenCipherFromBase64(os.Getenv("AUTH_PROVIDER_TOKEN_ENCRYPTION_KEY_BASE64"))
		if err != nil {
			logger.Error("invalid provider token encryption configuration", "error", err)
			os.Exit(2)
		}
		appleClient, err := auth.NewAppleOAuthClient(appleOAuthConfig)
		if err != nil {
			logger.Error("create Apple OAuth client failed", "error", err)
			os.Exit(2)
		}
		appleRevoker = appleClient
		appleLogin, err = auth.NewAppleLoginService(
			appleClient,
			appleClient,
			auth.NewGormAppleAccountResolver(databaseConnection.DB),
			tokenService,
			providerTokenCipher,
		)
		if err != nil {
			logger.Error("create Apple login service failed", "error", err)
			os.Exit(2)
		}
	}
	withdrawal, err := auth.NewAccountWithdrawalService(
		auth.NewGormWithdrawalAccountStore(databaseConnection.DB),
		providerTokenCipher,
		appleRevoker,
		sessionManager.CloseUserSessions,
	)
	if err != nil {
		logger.Error("create account withdrawal service failed", "error", err)
		os.Exit(2)
	}
	logger.Info(
		"authentication token service ready",
		"access_ttl", tokenConfig.AccessTTL,
		"refresh_idle_ttl", tokenConfig.RefreshTTL,
		"refresh_absolute_ttl", tokenConfig.RefreshAbsoluteTTL,
		"cors_allow_all_origins", originConfig.AllowAllOrigins,
		"cors_allowed_origins", originConfig.AllowedOrigins,
		"google_oauth_enabled", googleOAuthConfig.Enabled(),
		"apple_oauth_enabled", appleOAuthConfig.Enabled(),
	)

	userStatusChecker := auth.NewGormUserStatusChecker(databaseConnection.DB)
	application := server.New(
		cfg,
		logger,
		registry,
		sessionManager,
		aiPool,
		originConfig,
		auth.RequireUser(tokenService, userStatusChecker),
		func(ctx context.Context, raw string) (uuid.UUID, error) {
			return auth.AuthenticateUser(ctx, tokenService, userStatusChecker, raw)
		},
	)

	// AI_PRIVACY_ME_IMAGE_PATH에 지정된 기본 참조 얼굴을 등록한다.
	// 등록 시간이 HTTP 서버 시작을 막지 않도록 비동기로 실행한다.
	if aiPool != nil && cfg.AIMeImagePath != "" {
		go func() {
			data, err := os.ReadFile(cfg.AIMeImagePath)
			if err != nil {
				logger.Warn(
					"env reference face not readable",
					"path", cfg.AIMeImagePath,
					"error", err,
				)
				return
			}

			ctx, cancel := context.WithTimeout(
				context.Background(),
				130*time.Second,
			)
			defer cancel()

			if _, err := aiPool.AddWhitelist(
				ctx,
				"default",
				data,
			); err != nil {
				logger.Warn(
					"env reference face registration failed",
					"path", cfg.AIMeImagePath,
					"error", err,
				)
				return
			}

			logger.Info(
				"env reference face registered",
				"path", cfg.AIMeImagePath,
			)
		}()
	}

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           auth.MountAuthHTTPWithWithdrawal(application.Handler(), tokenService, googleLogin, appleLogin, withdrawal, logger, originConfig),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrors := make(chan error, 1)

	go func() {
		logger.Info(
			"InnoLive media server started",
			"address", cfg.HTTPAddr,
			"privacy_mode", cfg.PrivacyMode,
			"ai_targets", cfg.AITargets,
			"wire_format", cfg.AIWireFormat,
			"max_sessions", cfg.MaxSessions,
			"ffmpeg_spawn_concurrency",
			cfg.FFmpegSpawnConcurrency,
		)

		serverErrors <- httpServer.ListenAndServe()
	}()

	signalContext, stop := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer stop()

	select {
	case <-signalContext.Done():
		logger.Info("shutdown signal received")

	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error(
				"HTTP server stopped unexpectedly",
				"error", err,
			)
			os.Exit(1)
		}
	}

	shutdownContext, cancel := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
	defer cancel()

	if err := httpServer.Shutdown(shutdownContext); err != nil {
		logger.Error(
			"graceful HTTP shutdown failed",
			"error", err,
		)
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

	return slog.New(
		slog.NewJSONHandler(
			os.Stdout,
			&slog.HandlerOptions{
				Level: logLevel,
			},
		),
	)
}
