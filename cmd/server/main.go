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
	"inno-live-server/internal/streaming"
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
			"session ownership auth AND user auth are DISABLED " +
				"(INNOLIVE_REQUIRE_SESSION_AUTH=false); " +
				"any client can control or hijack any session — " +
				"for local development only",
		)
	}

	// 디코더 핀은 소스 방향을 따라가는데 EGRESS_VIDEO_SIZE는 고정 캔버스라,
	// 세로 세션이 가로 캔버스에 재수납되면서 다운스케일과 필러박스가 생긴다.
	// 가로 고정 인입이 필요한 운영도 있으므로 거부하지 않고 경고만 남긴다.
	if cfg.DecoderPinLongEdge > 0 && cfg.EgressVideoSize != "" {
		logger.Warn(
			"DECODER_PIN_LONG_EDGE and EGRESS_VIDEO_SIZE are both set; "+
				"a portrait session pinned by the decoder will be re-boxed into "+
				"the fixed egress canvas, losing resolution to letterboxing — "+
				"unset EGRESS_VIDEO_SIZE unless a fixed ingest canvas is required",
			"decoder_pin_long_edge", cfg.DecoderPinLongEdge,
			"egress_video_size", cfg.EgressVideoSize,
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
			cfg.DecoderPinLongEdge,
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
	emailAuthConfig, err := auth.LoadEmailAuthConfigFromEnv()
	if err != nil {
		logger.Error("invalid email authentication configuration", "error", err)
		os.Exit(2)
	}
	var emailLogin *auth.EmailAuthService
	if emailAuthConfig.Enabled() {
		pendingEmailSignupStore, err := auth.NewRedisPendingEmailSignupStore(context.Background(), emailAuthConfig)
		if err != nil {
			logger.Error("connect email signup Redis failed", "error", err)
			os.Exit(2)
		}
		defer func() {
			if err := pendingEmailSignupStore.Close(); err != nil {
				logger.Error("close email signup Redis failed", "error", err)
			}
		}()
		emailSender, err := auth.NewSMTPVerificationEmailSender(emailAuthConfig)
		if err != nil {
			logger.Error("create email verification sender failed", "error", err)
			os.Exit(2)
		}
		emailLogin, err = auth.NewEmailAuthService(
			pendingEmailSignupStore,
			auth.NewGormEmailAccountStore(databaseConnection.DB),
			emailSender,
			tokenService,
			emailAuthConfig,
		)
		if err != nil {
			logger.Error("create email authentication service failed", "error", err)
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
	youtubeOAuthConfig, err := auth.LoadYouTubeOAuthConfigFromEnv()
	if err != nil {
		logger.Error("invalid YouTube OAuth configuration", "error", err)
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
		"email_auth_enabled", emailAuthConfig.Enabled(),
		"youtube_streaming_enabled", youtubeOAuthConfig.Enabled(),
	)

	userStatusChecker := auth.NewGormUserStatusChecker(databaseConnection.DB)
	// YouTube 송출 연동. 암호화 키(cipher)는 Apple 활성 시 위에서 이미
	// 생성됐을 수 있으므로 없을 때만 만든다 — Apple 없이 YouTube만 켠
	// 배포에서도 refresh token 암호화가 성립해야 한다.
	var youtubeConnect *auth.YouTubeConnectService
	streamingProviders := map[auth.StreamingProvider]streaming.Provider{}
	// 송출 계정 저장소·조회 서비스는 플랫폼 중립이라 YouTube 설정 여부와
	// 무관하게 조립한다 — 연결이 없으면 조회가 빈 배열을 돌려줄 뿐이다.
	streamingAccountStore := auth.NewGormStreamingAccountStore(databaseConnection.DB)
	streamingDisconnectHooks := map[auth.StreamingProvider]auth.StreamingDisconnectHooks{}
	if youtubeOAuthConfig.Enabled() {
		if providerTokenCipher == nil {
			providerTokenCipher, err = auth.NewProviderTokenCipherFromBase64(os.Getenv("AUTH_PROVIDER_TOKEN_ENCRYPTION_KEY_BASE64"))
			if err != nil {
				logger.Error("invalid provider token encryption configuration", "error", err)
				os.Exit(2)
			}
		}
		youtubeOAuthClient, err := auth.NewYouTubeOAuthClient(youtubeOAuthConfig)
		if err != nil {
			logger.Error("create YouTube OAuth client failed", "error", err)
			os.Exit(2)
		}
		youtubeConnect, err = auth.NewYouTubeConnectService(
			youtubeOAuthClient,
			streamingAccountStore,
			userStatusChecker,
			providerTokenCipher,
		)
		if err != nil {
			logger.Error("create YouTube connect service failed", "error", err)
			os.Exit(2)
		}
		youtubeTokens, err := auth.NewYouTubeAccessTokenProvider(youtubeOAuthClient, streamingAccountStore, providerTokenCipher)
		if err != nil {
			logger.Error("create YouTube access token provider failed", "error", err)
			os.Exit(2)
		}
		youtubeProvider, err := streaming.NewYouTubeProvider(youtubeTokens, streamingAccountStore, providerTokenCipher)
		if err != nil {
			logger.Error("create YouTube streaming provider failed", "error", err)
			os.Exit(2)
		}
		streamingProviders[auth.StreamingProviderYouTube] = youtubeProvider
		// 해제 시 정리 훅: ①재사용 스트림 삭제(Live API) ②Google 권한 취소.
		streamingDisconnectHooks[auth.StreamingProviderYouTube] = auth.StreamingDisconnectHooks{
			CleanupResources: youtubeProvider.CleanupStreamingResources,
			RevokeToken:      youtubeOAuthClient.RevokeToken,
		}
	}
	// 조회·해제 서비스는 플랫폼 중립이라 훅 구성 뒤 한 번만 조립한다.
	streamingAccounts, err := auth.NewStreamingAccountService(streamingAccountStore, userStatusChecker, providerTokenCipher, streamingDisconnectHooks, logger)
	if err != nil {
		logger.Error("create streaming account service failed", "error", err)
		os.Exit(2)
	}
	// INNOLIVE_REQUIRE_SESSION_AUTH=false는 로컬 개발용으로 명시된 탈출구다
	// (위에서 크게 경고한다). 토큰 없는 벤치 도구(pion-load)가 개발 서버를
	// 상대로 계속 동작하도록 사용자 인증까지 함께 끈다. 프로덕션은 기본값
	// (true)을 유지하므로 영향이 없다.
	requireUser := auth.RequireUser(tokenService, userStatusChecker)
	authenticateUser := func(ctx context.Context, raw string) (uuid.UUID, error) {
		return auth.AuthenticateUser(ctx, tokenService, userStatusChecker, raw)
	}
	if !cfg.RequireSessionAuth {
		requireUser = nil
		authenticateUser = nil
	}
	application := server.New(
		cfg,
		logger,
		registry,
		sessionManager,
		aiPool,
		originConfig,
		requireUser,
		streamingProviders,
		authenticateUser,
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
		Handler:           auth.MountAuthHTTPWithStreaming(application.Handler(), tokenService, googleLogin, appleLogin, emailLogin, withdrawal, youtubeConnect, streamingAccounts, logger, originConfig, sessionManager.CloseUserSessionsForLogout),
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
