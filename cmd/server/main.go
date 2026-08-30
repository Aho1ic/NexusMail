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
	// Signal handling is the process's own concern, so it lives here rather than in
	// run: that keeps run to assembly and lets a test drive the same shutdown path
	// by cancelling a context instead of raising a signal at the whole test binary.
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(rootCtx); err != nil {
		slog.Error("nexusmail stopped", "error", err)
		os.Exit(1)
	}
}

func run(rootCtx context.Context) error {
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
	// Registered after syncer.Stop so it runs before it: defers are LIFO, and a
	// pending draft push must not fire into a supervisor that has already stopped.
	defer draftSvc.Close()
	go sender.Start(rootCtx)
	go maintenance(rootCtx, repo, blobStore, maintenanceInterval)

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

// maintenanceInterval is how often expired sessions are swept and the blob cache
// is trimmed. Both are cheap and neither is urgent: a session past its TTL is
// already rejected on use, and the cache only has to stay under its ceiling over
// time.
const maintenanceInterval = 15 * time.Minute

// maintenance takes its interval as a parameter so a test can observe a tick
// without waiting out the production cadence.
func maintenance(ctx context.Context, repo maintRepo, blobs evictor, every time.Duration) {
	ticker := time.NewTicker(every)
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
