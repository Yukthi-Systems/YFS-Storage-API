package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/api"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/config"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/middleware"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/rustapi"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/service/download"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/service/file"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/service/purge"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/service/session"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/service/upload"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/service/wopi"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/storage"
	_ "github.com/Yukthi-Systems/YFS-Storage-API/internal/storage/local"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/token"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/tus"
	"github.com/Yukthi-Systems/YFS-Storage-API/internal/utils"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// Load configuration.
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger, err := utils.NewLogger(cfg)
	if err != nil {
		return fmt.Errorf("initializing logger: %w", err)
	}
	store, err := storage.NewFromConfig(cfg)
	if err != nil {
		return fmt.Errorf("initializing storage driver %q: %w", cfg.StorageDriver, err)
	}
	logger.Info("storage driver initialized", "driver", cfg.StorageDriver)

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	defer rdb.Close()

	pingCtx, cancelPing := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPing()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		return fmt.Errorf("connecting to redis at %q: %w", cfg.RedisAddr, err)
	}
	logger.Info("redis session store connected", "addr", cfg.RedisAddr)

	issuer, err := token.NewIssuer(token.Config{Redis: rdb, EncryptionKey: cfg.TokenEncryptionKey})
	if err != nil {
		return fmt.Errorf("initializing token issuer: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	purgeDBPath := cfg.PurgeDBPath
	if purgeDBPath == "" {
		purgeDBPath = purge.DefaultDBPath()
	}
	purgeDB, err := purge.OpenDB(purgeDBPath)
	if err != nil {
		return fmt.Errorf("opening purge database: %w", err)
	}
	defer purgeDB.Close()
	logger.Info("purge queue database opened", "path", purgeDBPath)

	purgeQueue := purge.New(purgeDB, store, logger, cfg.DeleteWorkerCount)
	if err := purgeQueue.Start(ctx); err != nil {
		return fmt.Errorf("starting purge queue: %w", err)
	}

	rustClient := rustapi.New(rustapi.Config{
		BaseURL: cfg.RustCallbackBaseURL,
		APIKey:  cfg.RustCallbackAPIKey,
	})
	uploadManager := upload.NewManager(store, rustClient)

	tusHandler, err := tus.NewHandler(tus.Config{
		StagingDir:         cfg.TusStagingDir,
		BasePath:           cfg.TusBasePath,
		MaxSize:            cfg.TusMaxUploadSize,
		CorsAllowedOrigins: cfg.TusCorsAllowedOrigins,
		Issuer:             issuer,
		Manager:            uploadManager,
		Logger:             logger,
	})
	if err != nil {
		return fmt.Errorf("initializing tus handler: %w", err)
	}

	handlers := &api.Handlers{
		Issuer: issuer,
		Session: session.New(issuer, store, cfg.MaxStorageUsagePercent, session.URLBases{
			Upload:   cfg.TusBasePath,
			Download: "/download/",
			WOPI:     "/wopi/files/",
		}),
		File:                       file.New(store),
		Download:                   download.New(store),
		Purge:                      purgeQueue,
		Tus:                        tusHandler,
		RustAPIToken:               cfg.RustAPIToken,
		StorageBackoff:             cfg.StorageBackoff,
		MaxUploadBatchSize:         cfg.MaxUploadBatchSize,
		MaxDeleteBatchSize:         cfg.MaxDeleteBatchSize,
		DownloadCorsAllowedOrigins: cfg.DownloadCorsAllowedOrigins,
		WOPISessionTTL:             cfg.WOPISessionTTL,
		Logger:                     logger,
		WOPI:                       wopi.New(store, wopi.NewMemoryLockStore(), 30*time.Minute, rustClient),
	}
	mux := http.NewServeMux()
	handlers.Register(mux, cfg.TusBasePath)

	var handler http.Handler = mux
	handler = middleware.RequestLogging(logger)(handler)
	handler = middleware.Recovery(logger)(handler)

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      handler,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.ListenAddr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serving: %w", err)
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}
