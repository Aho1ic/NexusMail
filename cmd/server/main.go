package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
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
	// The account service and the OAuth manager would form a cycle if either took the
	// other at construction, so the manager is handed its credential store here.
	// Without it a rotated refresh token lives only in the manager's cache and the
	// next boot re-authorizes with a token the provider has already invalidated.
	oauthManager.SetCredentialStore(accountSvc)
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
	// The workers are joined before the deferred repo.Close, so this defer is
	// registered after it: defers are LIFO. The SMTP client only uses ctx for the
	// dial and drives the rest of the conversation on socket deadlines, so a
	// cancelled context does not abort a delivery in flight — without the join its
	// CreateSentMessage and SetDraftDelivery writes would run against a closed
	// database.
	//
	// The join keeps the database open; it does not make workerCtx usable. A
	// cancelled context fails a write in database/sql before a connection is even
	// taken from the pool, so the send worker derives its own context for the
	// writes that record a delivery outcome (see postDeliveryWrite). Both halves
	// are needed: without either, a message the provider accepted is left in
	// 'sending' for RecoverSendingDrafts to downgrade to 'unknown' on the next
	// boot, and presented to the user as possibly undelivered.
	//
	// The workers get their own cancel rather than watching rootCtx, because a
	// listener that fails to bind returns from run with rootCtx still live, and Wait
	// would then block forever.
	workerCtx, stopWorkers := context.WithCancel(rootCtx)
	var workers sync.WaitGroup
	defer func() {
		stopWorkers()
		workers.Wait()
	}()
	workers.Add(2)
	go func() {
		defer workers.Done()
		sender.Start(workerCtx)
	}()
	go func() {
		defer workers.Done()
		maintenance(workerCtx, repo, blobStore, maintenanceInterval)
	}()

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

// evictor is the blob maintenance surface, which is deliberately wider here than
// ports.BlobStore: ReclaimOrphans scans for unreferenced rows, and no HTTP handler
// should be able to reach a full-table sweep. The ticker is its only caller.
type evictor interface {
	Evict(context.Context) error
	ReclaimOrphans(context.Context) error
}

// maintenanceInterval is how often expired sessions are swept, the blob cache is
// trimmed and orphaned durable blobs are reclaimed. None is urgent: a session past
// its TTL is already rejected on use, the cache only has to stay under its ceiling
// over time, and an orphan costs disk rather than correctness.
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
			// Evict only ever considers durability='cache', so this is the sole
			// reclamation path for the durable tier: without it every attachment
			// ever uploaded to a draft stays on disk for the life of the deployment.
			_ = blobs.ReclaimOrphans(ctx)
		}
	}
}
