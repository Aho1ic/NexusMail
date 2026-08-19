package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"nexusmail/internal/config"
	"nexusmail/internal/platform/cryptobox"
	imapprovider "nexusmail/internal/provider/imap"
	"nexusmail/internal/provider/oauth"
	smtpprovider "nexusmail/internal/provider/smtp"
	"nexusmail/internal/realtime"
	"nexusmail/internal/repository/sqlite"
	accountservice "nexusmail/internal/service/account"
	draftservice "nexusmail/internal/service/draft"
	messageservice "nexusmail/internal/service/message"
	sendservice "nexusmail/internal/service/send"
	sessionservice "nexusmail/internal/service/session"
	"nexusmail/internal/storage"
	httptransport "nexusmail/internal/transport/http"
	"nexusmail/internal/version"
)

func main() {
	if err := run(); err != nil {
		slog.Error("nexusmail stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	if cfg.LogLevel == "debug" {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	slog.Info("nexusmail starting", "version", version.Value)

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	repo, err := sqlite.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer repo.Close()
	box, err := cryptobox.New(cfg.MasterKey)
	if err != nil {
		return err
	}
	blobStore, err := storage.New(filepath.Join(cfg.DataDir, "blobs"), cfg.BlobCacheBytes, repo)
	if err != nil {
		return err
	}
	hub := realtime.New()
	accountSvc := accountservice.New(repo, box)
	oauthManager := oauth.New(cfg)
	syncer := imapprovider.NewSupervisor(repo, blobStore, accountSvc, oauthManager, hub)
	messageSvc := messageservice.New(repo, syncer, hub)
	draftSvc := draftservice.New(repo, hub, syncer)
	sessionSvc := sessionservice.New(repo, cfg.APIKey, cfg.SessionIdleTTL, cfg.SessionMaxTTL)
	smtpClient := smtpprovider.New(45 * time.Second)
	sender := sendservice.New(repo, blobStore, accountSvc, oauthManager, smtpClient, hub, cfg.MaxOutboundBytes, syncer)
	if err := syncer.Start(rootCtx); err != nil {
		return err
	}
	defer syncer.Stop()
	go sender.Start(rootCtx)
	go maintenance(rootCtx, repo, blobStore)

	api := httptransport.New(cfg, repo, blobStore, accountSvc, messageSvc, draftSvc, sessionSvc, oauthManager, syncer, sender, hub, rootCtx)
	server := &http.Server{Addr: cfg.ListenAddr, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 60 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("nexusmail listening", "address", cfg.ListenAddr)
		if serveErr := server.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()
	select {
	case <-rootCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case serveErr := <-errCh:
		return serveErr
	}
}

type maintRepo interface {
	DeleteExpiredSessions(context.Context, int64) error
}
type evictor interface{ Evict(context.Context) error }

func maintenance(ctx context.Context, repo maintRepo, blobs evictor) {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = repo.DeleteExpiredSessions(ctx, time.Now().UnixMilli())
			_ = blobs.Evict(ctx)
		}
	}
}
