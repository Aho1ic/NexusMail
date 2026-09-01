package imap

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
	accountservice "nexusmail/internal/service/account"
)

// Supervisor is the IMAP provider: it owns one command connection and one IDLE
// connection per account and is shared by the message, draft and send services.
// This file holds the type and its lifecycle; the per-responsibility pieces live
// in the sibling files (loop, conn, sync, reconcile, ingest, body, actions,
// remotedrafts) and are all part of this package.

type TokenProvider interface {
	AccessToken(context.Context, domain.Account, string) (string, error)
}

type Supervisor struct {
	repo         ports.Repository
	blobs        ports.BlobStore
	accounts     *accountservice.Service
	tokens       TokenProvider
	events       ports.Publisher
	mu           sync.RWMutex
	runtimes     map[int64]*runtime
	wg           sync.WaitGroup
	bodyQueue    chan int64
	bodySlots    chan struct{}
	bodySeen     sync.Map
	workerCancel context.CancelFunc
	// bodyAttempts counts failed prefetch attempts per message so a body that can
	// never be fetched stops being retried. It is memory-only: a restart is a fine
	// time to try again, and persisting it would need a migration the runner
	// cannot apply.
	bodyAttempts sync.Map
	// lastReconcile tracks when each mailbox last had its flags and deletions
	// checked against the provider.
	lastReconcile sync.Map
	// lastSyncReq throttles RequestMailbox so a passive reader hitting the
	// feed in a tight loop (or the realtime 80ms-coalesced refresh in App.tsx)
	// does not push a fresh syncReq every tick. The entry is set whenever a
	// request is enqueued, regardless of whether the chan accepted it; a missed
	// drop still counts because the supervisor would have processed the
	// previous one.
	lastSyncReq sync.Map
	// dial overrides transport establishment in tests; nil uses TLS to the account host.
	dial func(context.Context, domain.Account) (net.Conn, error)
	// commandStall overrides commandStallWindow so a test can drive the recovery
	// path without waiting out the production window.
	commandStall time.Duration
	// dropIdleNotifications simulates providers that advertise IDLE but never
	// deliver EXISTS, so tests can exercise the polling safety net.
	dropIdleNotifications bool
	// commandRefresh overrides commandRefreshInterval so a test can drive the
	// scheduled rebuild without waiting out the production interval.
	commandRefresh time.Duration
	// authRetry overrides authRetryBackoff so a test can walk an account to
	// maxAuthFailures without waiting out the production window.
	authRetry time.Duration
	// failProbe makes probeInbox fail without disturbing the socket, so a test can
	// exercise the dead-but-open connection that client.Closed() never reports.
	failProbe atomic.Bool
}

func NewSupervisor(repo ports.Repository, blobs ports.BlobStore, accounts *accountservice.Service, tokens TokenProvider, events ports.Publisher) *Supervisor {
	return &Supervisor{
		repo: repo, blobs: blobs, accounts: accounts, tokens: tokens, events: events,
		runtimes: make(map[int64]*runtime), bodyQueue: make(chan int64, 256), bodySlots: make(chan struct{}, 4),
	}
}

func (s *Supervisor) Start(ctx context.Context) error {
	accounts, err := s.repo.ListAccounts(ctx)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		s.StartAccount(ctx, account)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.workerCancel = cancel
	s.mu.Unlock()
	for range 4 {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			// bodyWorker has no recover of its own: a panic in MIME parsing or
			// the FTS5 update trigger would terminate the whole process and
			// lose the 2 IMAP loops, the send worker, and the other 3 body
			// workers. Recover here so one bad message cannot take the server
			// down.
			defer func() {
				if r := recover(); r != nil {
					slog.Error("body worker panicked", "panic", r, "stack", string(debug.Stack()))
				}
			}()
			s.bodyWorker(workerCtx)
		}()
	}
	return nil
}

func (s *Supervisor) StartAccount(parent context.Context, account domain.Account) {
	s.mu.Lock()
	if _, exists := s.runtimes[account.ID]; exists {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	rt := &runtime{account: account, cancel: cancel, syncReq: make(chan int64, 8)}
	s.runtimes[account.ID] = rt
	s.mu.Unlock()
	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.commandLoop(ctx, rt) }()
	go func() { defer s.wg.Done(); s.idleLoop(ctx, rt) }()
}

func (s *Supervisor) Stop() {
	s.mu.Lock()
	if s.workerCancel != nil {
		s.workerCancel()
	}
	for _, rt := range s.runtimes {
		rt.cancel()
		if client := rt.client.Load(); client != nil {
			_ = client.Close()
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Supervisor) runtime(accountID int64) (*runtime, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rt := s.runtimes[accountID]
	if rt == nil {
		return nil, errors.New("account runtime is not started")
	}
	return rt, nil
}
