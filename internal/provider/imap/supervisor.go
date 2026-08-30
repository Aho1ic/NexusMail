package imap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"mime"
	"net"
	"net/mail"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nexusmail/internal/domain"
	mailparser "nexusmail/internal/mail"
	"nexusmail/internal/ports"
	"nexusmail/internal/provider"
	providerauth "nexusmail/internal/provider/auth"
	accountservice "nexusmail/internal/service/account"
	"nexusmail/internal/version"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	messagecharset "github.com/emersion/go-message/charset"
	"github.com/emersion/go-sasl"
)

type TokenProvider interface {
	AccessToken(context.Context, domain.Account, string) (string, error)
}

type runtime struct {
	account domain.Account
	cancel  context.CancelFunc
	syncReq chan int64
	cmdMu   sync.Mutex
	// urgent counts foreground waiters for cmdMu. Background body prefetch
	// yields while it is non-zero so a new-mail sync never queues behind a
	// backlog of body fetches.
	urgent atomic.Int32
	client atomic.Pointer[imapclient.Client]
}

// lock claims the command connection for sync or user-facing work.
func (rt *runtime) lock() {
	rt.urgent.Add(1)
	rt.cmdMu.Lock()
	rt.urgent.Add(-1)
}

func (rt *runtime) unlock() { rt.cmdMu.Unlock() }

// lockBackground claims the command connection for opportunistic prefetch and
// steps aside whenever foreground work is waiting.
func (rt *runtime) lockBackground(ctx context.Context) bool {
	for ctx.Err() == nil {
		if rt.urgent.Load() == 0 {
			rt.cmdMu.Lock()
			if rt.urgent.Load() == 0 {
				return true
			}
			rt.cmdMu.Unlock()
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(25 * time.Millisecond):
		}
	}
	return false
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
	// failProbe makes probeInbox fail without disturbing the socket, so a test can
	// exercise the dead-but-open connection that client.Closed() never reports.
	failProbe atomic.Bool
}

const maxInlineDraftImportBytes = 1 << 20

// realtimePollInterval is a safety net for IMAP servers that advertise IDLE
// but delay or drop mailbox change notifications. IDLE still handles the
// normal path; this bounds the worst-case inbox discovery latency.
const realtimePollInterval = 5 * time.Second

const periodicSyncInterval = 5 * time.Minute

// bulkFlagChunk caps the UIDs per STORE so a large mark-read stays inside the
// command line limits servers enforce and yields the connection between chunks.
const bulkFlagChunk = 500

// reconcileInterval bounds how often a mailbox is checked for changes the
// append-only path cannot see: flags set in another client and messages deleted
// or expunged elsewhere. It costs one UID FETCH of flags over the locally stored
// UIDs, so it runs on the periodic sync rather than on the 5s probe.
const reconcileInterval = 5 * time.Minute

// backgroundReconcileInterval is the same for mailboxes the user is not watching.
// It has to exceed periodicSyncInterval to be worth anything: at equal intervals
// reconciliation is due on every tick, so every mailbox is selected and walked
// every time and the cheap STATUS check can never skip one. Mail read or deleted
// in another client still converges here, just not on the inbox's schedule.
const backgroundReconcileInterval = 30 * time.Minute

// reconcileIntervalFor returns the reconciliation cadence for a mailbox. The gate
// in reconcileMailbox and the skip in mailboxQuiet must agree on it, or a mailbox
// would be skipped as quiet and then never reconciled.
func reconcileIntervalFor(role string) time.Duration {
	if role == "inbox" {
		return reconcileInterval
	}
	return backgroundReconcileInterval
}

// reconcileFlagChunk caps the UIDs per flag FETCH during reconciliation.
const reconcileFlagChunk = 2000

// slowPhaseThreshold is how long a stretch of command-connection work may take
// before it is logged. Everything on that connection is serialised, so anything
// slow here is new-mail latency for the whole account, and the log line is what
// turns "mail is late" into a specific phase and mailbox.
const slowPhaseThreshold = 3 * time.Second

// observe times a stretch of command-connection work and logs it when it runs
// long enough to be felt.
func observe(phase string, accountID int64, attrs ...any) func() {
	start := time.Now()
	return func() {
		elapsed := time.Since(start)
		if elapsed < slowPhaseThreshold {
			return
		}
		slog.Warn("imap phase held the command connection",
			append([]any{"phase", phase, "account_id", accountID, "elapsed", elapsed}, attrs...)...)
	}
}

// maxBodyAttempts bounds how often the prefetch retries one message. The
// candidate query returns everything that is not 'ready', which includes 'error',
// so without a cap a body that can never be fetched — the message was deleted
// remotely, or the provider refuses the part — was retried every 5 seconds for
// the life of the process, taking the command connection each time.
const maxBodyAttempts = 3

// commandRefreshInterval bounds how long one command connection is trusted.
// It exists because a stale connection is silent rather than broken: QQ stopped
// reporting new mail on a long-lived connection through both STATUS and SELECT,
// so every detection path agreed there was nothing to fetch and mail waited tens
// of minutes for something to replace the connection. Ten minutes bounds that
// worst case at roughly one periodic sync while costing one extra LOGIN per
// account per ten minutes — negligible next to the 720 STATUS probes an hour the
// same connection already sends.
//
// It is deliberately not provider-specific: a connection that has been up for
// ten minutes is not more valuable than a fresh one on any provider, and the
// providers that need this are exactly the ones least likely to document it.
const commandRefreshInterval = 10 * time.Minute

// commandRefreshJitter spreads the rebuild so accounts do not reconnect in
// lockstep after a restart.
const commandRefreshJitter = 2 * time.Minute

// commandRefreshOrDefault returns the interval before the command connection is
// rebuilt, jittered. Tests override the base to drive the path in seconds.
func (s *Supervisor) commandRefreshOrDefault() time.Duration {
	base := commandRefreshInterval
	jitter := int64(commandRefreshJitter)
	if s.commandRefresh > 0 {
		base = s.commandRefresh
		// Keep the override dominant: a fixed 2-minute spread would swamp a
		// 2-second test interval and the rebuild would never be observed.
		jitter = int64(base) / 4
	}
	return base + time.Duration(rand.Int64N(jitter+1))
}

// maxProbeFailures is how many consecutive inbox probes may fail before the
// command connection is torn down and rebuilt. Three at realtimePollInterval is
// ~15 seconds of evidence, which is long enough not to react to one transient
// error and short enough that new mail is not waiting for the 5-minute periodic
// sync to notice the connection is dead.
const maxProbeFailures = 3

// authBackoff is the retry delay after the provider rejected the credentials.
// Reconnecting every second with a password the server already refused is how
// accounts get locked out, and no amount of retrying will fix it.
const authBackoff = 15 * time.Minute

// rateLimitBackoff is the retry delay after the provider throttled the account
// (e.g. QQ's "NO System busy!", 163's "Too many connections"). The 1s→5m
// network-error ladder tightens the throttle instead of clearing it, and on
// per-account throttle windows 5 min is not enough for the window to roll
// over; 15 min matches auth_error and gives the provider's window time to
// reset. The probe path also exits the inner loop on rate-limit so a single
// rejection is not followed by another 5-second STATUS hitting the same
// closed window.
const rateLimitBackoff = 15 * time.Minute

// otpFreshness bounds how old a message may be for its verification code to ride
// on a realtime event. A first sync imports 30 days of mail and the body prefetch
// walks the whole backlog, both of which look like arrivals to the event stream;
// without this the browser would raise a persistent notification for every code
// the account ever received. Codes expire in minutes, so an older one is useless
// anyway and is still reachable from the message detail.
const otpFreshness = 10 * time.Minute

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

func (s *Supervisor) commandLoop(ctx context.Context, rt *runtime) {
	backoff := time.Second
	for ctx.Err() == nil {
		_ = s.repo.UpdateAccountStatus(ctx, rt.account.ID, "connecting", nil)
		client, err := s.connect(ctx, rt.account, nil, s.commandStallOrDefault())
		if err != nil {
			// Credentials the server already refused will be refused again, so the
			// account is parked in auth_error with a long delay instead of being
			// hammered on the connection ladder. A throttled connect likewise
			// needs the full rateLimitBackoff: reconnecting every second keeps
			// the throttle engaged.
			status, delay := "backoff", backoff
			switch {
			case isAuthFailure(err):
				status, delay = "auth_error", authBackoff
			case isRateLimited(err):
				status, delay = "backoff", rateLimitBackoff
			}
			s.setError(ctx, rt.account.ID, status, err)
			if !waitBackoff(ctx, delay) {
				return
			}
			if status == "backoff" {
				backoff = min(backoff*2, 5*time.Minute)
			}
			continue
		}
		rt.client.Store(client)
		_ = s.repo.UpdateAccountStatus(ctx, rt.account.ID, "syncing", nil)
		// Refresh the catalog without holding the command lock so the LIST
		// round-trip and mailbox upserts do not block the new-mail path.
		if _, listErr := s.refreshMailboxCatalog(ctx, rt, client); listErr != nil {
			s.setError(ctx, rt.account.ID, "backoff", listErr)
			s.closeCommand(rt, client)
			if !waitBackoff(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 5*time.Minute)
			continue
		}
		rt.lock()
		syncErr := s.syncAllMailboxes(ctx, rt, client)
		rt.unlock()
		if syncErr != nil {
			// OAuth tokens can expire mid-session: the connect succeeded but a
			// later IMAP command now returns ResponseCodeAuthenticationFailed.
			// Without this re-check the error flows into the 1s-5m ladder and
			// hammers the provider, bypassing the auth_error park that exists
			// on the connect path. A "System busy" / "Too many connections"
			// from the provider is the same shape of mistake: the 1s ladder
			// keeps the throttle engaged, so it gets the dedicated
			// rateLimitBackoff instead.
			syncStatus, syncDelay := "backoff", backoff
			switch {
			case isAuthFailure(syncErr):
				syncStatus, syncDelay = "auth_error", authBackoff
			case isRateLimited(syncErr):
				syncStatus, syncDelay = "backoff", rateLimitBackoff
			}
			// A sync that keeps failing used to reconnect with no delay at all,
			// because the ladder was reset on connect and only the connect path
			// waited. That is a tight reconnect loop against the provider.
			s.setError(ctx, rt.account.ID, syncStatus, syncErr)
			s.closeCommand(rt, client)
			if !waitBackoff(ctx, syncDelay) {
				return
			}
			backoff = min(backoff*2, 5*time.Minute)
			continue
		}
		// The ladder is reset only once a sync has actually succeeded: reaching the
		// greeting proves nothing about whether the account can be read.
		backoff = time.Second
		s.enqueueBodyCandidates(ctx, rt.account.ID)
		_ = s.repo.UpdateAccountStatus(ctx, rt.account.ID, "connected", nil)
		ticker := time.NewTicker(periodicSyncInterval)
		// The command connection owns sync and is always alive, so the safety
		// net lives here rather than inside the IDLE state.
		probe := time.NewTicker(realtimePollInterval)
		// Rebuild the command connection periodically. A long-lived connection to
		// QQ stops reflecting new mail: neither the 5-second STATUS probe nor the
		// 5-minute SELECT sees it, so mail sat unnoticed for tens of minutes and
		// then arrived in one batch the moment the connection was replaced —
		// measured on this account as four messages spanning 41 minutes of
		// arrivals all stored in the same second, and a 42-minute delay that
		// resolved 6 seconds after an unrelated reconnect. The IDLE connection
		// already refreshes on its own timer for the same reason; the command
		// connection had no equivalent and could stay stale indefinitely.
		refresh := time.NewTimer(s.commandRefreshOrDefault())
		// probeFailures counts consecutive probe errors on this connection. A
		// probe is one STATUS on a connection that just authenticated, so
		// repeated failures mean the connection is no longer usable even when
		// go-imap has not noticed it closed — a socket the host's sleep or a NAT
		// dropped without an RST fails instantly and forever, and the old code
		// only logged that at Debug and waited for client.Closed().
		probeFailures := 0
		connected := true
		for connected {
			select {
			case <-ctx.Done():
				connected = false
			case <-probe.C:
				rt.lock()
				probeErr := s.probeInbox(ctx, rt, client)
				rt.unlock()
				if probeErr == nil {
					probeFailures = 0
					s.enqueueBodyCandidates(ctx, rt.account.ID)
				} else if isRateLimited(probeErr) {
					// A probe that hits the throttle must not be followed by
					// another one 5 seconds later: each STATUS in the throttle
					// window extends the provider's lockout. Drop out of the
					// inner loop so the connect path applies rateLimitBackoff
					// instead of running the probe ticker through the window.
					slog.Debug("mail inbox probe rate-limited, backing off", "account_id", rt.account.ID, "error", probeErr)
					connected = false
				} else {
					// A single failure must not tear down the connection: a
					// missing or misclassified inbox is a local problem that
					// reconnecting cannot fix. Sustained failure is different —
					// it is the signature of a connection that is dead without
					// being closed, and the only way out is a reconnect.
					probeFailures++
					slog.Debug("mail inbox probe failed", "account_id", rt.account.ID, "failures", probeFailures, "error", probeErr)
					if probeFailures >= maxProbeFailures {
						slog.Warn("mail inbox probe failing repeatedly, reconnecting",
							"account_id", rt.account.ID, "failures", probeFailures, "error", probeErr)
						connected = false
					}
				}
			case mailboxID := <-rt.syncReq:
				rt.lock()
				var err error
				if mailboxID == 0 {
					err = s.syncRole(ctx, client, rt.account.ID, "inbox", false)
				} else {
					mailbox, mailboxErr := s.repo.GetMailbox(ctx, mailboxID)
					if mailboxErr != nil {
						err = mailboxErr
					} else if mailbox.AccountID == rt.account.ID {
						err = s.syncMailbox(ctx, client, mailbox, false)
					}
				}
				rt.unlock()
				if err == nil {
					s.enqueueBodyCandidates(ctx, rt.account.ID)
				}
				if err != nil {
					connected = false
				}
			case <-ticker.C:
				// Refresh the catalog without holding the command lock so a
				// slow LIST cannot stall the new-mail probe for the whole
				// 5-minute interval.
				if _, listErr := s.refreshMailboxCatalog(ctx, rt, client); listErr != nil {
					slog.Debug("mailbox catalog refresh failed", "account_id", rt.account.ID, "error", listErr)
				}
				rt.lock()
				err := s.syncAllMailboxes(ctx, rt, client)
				rt.unlock()
				if err == nil {
					s.enqueueBodyCandidates(ctx, rt.account.ID)
				}
				if err != nil {
					connected = false
				}
			case <-refresh.C:
				// A scheduled rebuild, not a failure: leave the ladder and the
				// account status alone so this never looks like an outage.
				slog.Debug("refreshing command connection", "account_id", rt.account.ID)
				connected = false
			case <-client.Closed():
				connected = false
			}
		}
		ticker.Stop()
		probe.Stop()
		if !refresh.Stop() {
			select {
			case <-refresh.C:
			default:
			}
		}
		s.closeCommand(rt, client)
	}
}

func (s *Supervisor) idleLoop(ctx context.Context, rt *runtime) {
	backoff := time.Second
	for ctx.Err() == nil {
		updates := make(chan struct{}, 1)
		client, err := s.connect(ctx, rt.account, &imapclient.UnilateralDataHandler{Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data.NumMessages != nil && !s.dropIdleNotifications {
				select {
				case updates <- struct{}{}:
				default:
				}
			}
		}}, idleStallWindow)
		// retry closes the connection and waits before the next attempt. Every
		// post-connect failure below has to route through it: reaching the
		// greeting proves the socket works, not that the account can be idled,
		// and a bare `continue` here reconnected as fast as TLS handshakes
		// complete — with a full LOGIN each time. That traffic shape is what a
		// per-account throttle exists to punish, so a provider that
		// authenticates and then rejects SELECT (QQ's "System busy") was kept
		// permanently throttled by this loop, which also blocked the command
		// loop's own recovery no matter how long it backed off.
		retry := func(client *imapclient.Client, cause error) bool {
			if client != nil {
				_ = client.Close()
			}
			delay := retryDelay(cause, backoff)
			if cause != nil {
				slog.Debug("mail idle connection retrying", "account_id", rt.account.ID, "delay", delay, "error", cause)
			}
			if !waitBackoff(ctx, delay) {
				return false
			}
			backoff = min(backoff*2, 5*time.Minute)
			return true
		}
		if err != nil {
			if !retry(nil, err) {
				return
			}
			continue
		}
		inbox, err := s.repo.GetMailboxByRole(ctx, rt.account.ID, "inbox")
		if err != nil {
			// The command loop has not recorded mailboxes yet. This is a local
			// read that costs the provider nothing, so it keeps the quick retry
			// and leaves the ladder alone.
			_ = client.Close()
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		if _, err := client.Select(inbox.RemoteName, nil).Wait(); err != nil {
			if !retry(client, err) {
				return
			}
			continue
		}
		if !client.Caps().Has(goimap.CapIdle) {
			_ = client.Close()
			if !s.pollWithoutIdle(ctx, rt) {
				return
			}
			continue
		}
		idle, err := client.Idle()
		if err != nil {
			if !retry(client, err) {
				return
			}
			continue
		}
		// The ladder resets only once the connection is actually idling. Doing
		// it right after connect meant a loop that failed at SELECT every time
		// always retried at the 1-second floor and never climbed.
		backoff = time.Second
		refresh := time.NewTimer(time.Duration(20+rand.IntN(6)) * time.Minute)
		active := true
		for active {
			select {
			case <-ctx.Done():
				_ = client.Close()
				active = false
			case <-updates:
				s.requestSync(rt)
			case <-refresh.C:
				active = false
			case <-client.Closed():
				active = false
			}
		}
		if !refresh.Stop() {
			select {
			case <-refresh.C:
			default:
			}
		}
		_ = idle.Close()
		_ = idle.Wait()
		_ = client.Close()
	}
}

// probeInbox cheaply checks whether the inbox moved before paying for a sync.
// STATUS needs one round trip and leaves the selected mailbox untouched, so the
// safety net can run often without loading the provider. The caller must hold
// the command lock.
func (s *Supervisor) probeInbox(ctx context.Context, rt *runtime, client *imapclient.Client) error {
	mailbox, err := s.repo.GetMailboxByRole(ctx, rt.account.ID, "inbox")
	if err != nil {
		return err
	}
	if s.failProbe.Load() {
		// Test hook: fail the probe while leaving the socket healthy, which is
		// what a NAT-dropped or sleep-severed connection looks like to this loop.
		return errors.New("probe failed by test hook")
	}
	status, err := client.Status(mailbox.RemoteName, &goimap.StatusOptions{UIDNext: true, UIDValidity: true}).Wait()
	if err != nil {
		return err
	}
	unchanged := mailbox.UIDNext != nil && status.UIDNext != 0 &&
		uint32(status.UIDNext) == *mailbox.UIDNext && status.UIDValidity == mailbox.UIDValidity
	if unchanged {
		return nil
	}
	// The moment the provider first admitted the mailbox moved. Without this the
	// only timestamps available are the message's InternalDate and the row's
	// created_at, which cannot distinguish "the provider told us late" from "we
	// asked late" — the two have opposite fixes.
	stored := uint32(0)
	if mailbox.UIDNext != nil {
		stored = *mailbox.UIDNext
	}
	slog.Debug("mail inbox probe saw movement", "account_id", rt.account.ID,
		"status_uidnext", uint32(status.UIDNext), "stored_uidnext", stored)
	// skipReconcile=true: the probe's only job is to surface new mail fast. The
	// 5-minute periodic sync reconciles flag changes and remote expunges.
	return s.syncMailbox(ctx, client, mailbox, true)
}

// pollWithoutIdle drives sync signals for servers lacking IDLE. It returns
// false when the context is done, and true to re-probe capabilities.
func (s *Supervisor) pollWithoutIdle(ctx context.Context, rt *runtime) bool {
	ticker := time.NewTicker(realtimePollInterval)
	defer ticker.Stop()
	recheck := time.NewTimer(periodicSyncInterval)
	defer recheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			s.requestSync(rt)
		case <-recheck.C:
			return true
		}
	}
}

// stallGuard fails a connection that stops delivering data while the client is
// waiting on it.
//
// go-imap arms a read deadline only once a response has started arriving and
// clears it again afterwards, and cmd.Wait() has no timeout of its own. A
// provider that accepts a command and then goes quiet — throttled, or a socket a
// NAT dropped without ever sending a RST — therefore blocks the caller forever.
// On the command connection that caller holds cmdMu and is the command loop
// itself, so the account froze completely: the loop never returned to its select,
// so neither the 5s probe nor client.Closed() could recover it, and mail stopped
// appearing until the process was restarted. Bounding silence rather than total
// duration is what keeps a legitimately long sync working: that keeps delivering
// data, a dead connection does not.
type stallGuard struct {
	net.Conn
	// window is nanoseconds of tolerated silence, swapped once the connection is
	// established: setup is always quick, while an established connection may be
	// deliberately quiet for a long time.
	window atomic.Int64
}

func newStallGuard(conn net.Conn, window time.Duration) *stallGuard {
	guard := &stallGuard{Conn: conn}
	guard.window.Store(int64(window))
	return guard
}

func (c *stallGuard) Read(payload []byte) (int, error) {
	// Re-armed per read, so any byte of progress buys another full window. Set
	// after go-imap's own deadline calls, so this is the one that applies.
	_ = c.Conn.SetReadDeadline(time.Now().Add(time.Duration(c.window.Load())))
	return c.Conn.Read(payload)
}

func (c *stallGuard) Write(payload []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(time.Duration(c.window.Load())))
	return c.Conn.Write(payload)
}

// setupStallWindow bounds silence during the TLS handshake, the greeting and
// authentication. Those are never slow on a healthy connection, so they do not
// need the long window an established IDLE connection does — and giving them that
// window would let a half-open socket hold the account in "connecting" for as long
// as the window lasts.
const setupStallWindow = 30 * time.Second

// commandStallWindow bounds silence on the command connection. Every command it
// runs is either a user action or a sync, so a stall here is visible latency and
// reconnecting is always better than waiting.
const commandStallWindow = 90 * time.Second

// idleStallWindow bounds silence on the IDLE connection, which is legitimately
// quiet between arrivals. It only has to outlast the refresh cycle; a dead IDLE
// connection costs nothing beyond falling back to the 5s probe.
const idleStallWindow = 30 * time.Minute

func (s *Supervisor) commandStallOrDefault() time.Duration {
	if s.commandStall > 0 {
		return s.commandStall
	}
	return commandStallWindow
}

func (s *Supervisor) connect(ctx context.Context, account domain.Account, handler *imapclient.UnilateralDataHandler, stall time.Duration) (*imapclient.Client, error) {
	credential, err := s.accounts.Credential(account)
	if err != nil {
		return nil, err
	}
	options := &imapclient.Options{
		TLSConfig: &tls.Config{ServerName: account.IMAPHost, MinVersion: tls.VersionTLS12},
		Dialer:    &net.Dialer{Timeout: 20 * time.Second}, UnilateralDataHandler: handler,
		WordDecoder: &mime.WordDecoder{CharsetReader: messagecharset.Reader},
	}
	address := net.JoinHostPort(account.IMAPHost, strconv.Itoa(account.IMAPPort))
	// Bounded tightly for setup and widened to the caller's window once the
	// connection is authenticated and handed back.
	setup := min(stall, setupStallWindow)
	var guard *stallGuard
	var client *imapclient.Client
	switch {
	case s.dial != nil:
		conn, dialErr := s.dial(ctx, account)
		if dialErr != nil {
			return nil, dialErr
		}
		guard = newStallGuard(conn, setup)
		client = imapclient.New(guard, options)
	case account.IMAPTLSMode == "starttls":
		// The guard wraps the raw socket and STARTTLS layers on top of it, so the
		// deadline still governs every byte the TLS layer reads.
		conn, dialErr := options.Dialer.DialContext(ctx, "tcp", address)
		if dialErr != nil {
			return nil, dialErr
		}
		guard = newStallGuard(conn, setup)
		client, err = imapclient.NewStartTLS(guard, options)
	default:
		conn, dialErr := options.Dialer.DialContext(ctx, "tcp", address)
		if dialErr != nil {
			return nil, dialErr
		}
		guard = newStallGuard(conn, setup)
		secure := tls.Client(guard, options.TLSConfig)
		if handshakeErr := secure.HandshakeContext(ctx); handshakeErr != nil {
			_ = conn.Close()
			return nil, handshakeErr
		}
		client = imapclient.New(secure, options)
	}
	if err != nil {
		return nil, err
	}
	if account.AuthType == "oauth2" {
		token, tokenErr := s.tokens.AccessToken(ctx, account, credential.RefreshToken)
		if tokenErr != nil {
			_ = client.Close()
			return nil, tokenErr
		}
		err = client.Authenticate(&providerauth.XOAUTH2{Username: account.Username, AccessToken: token})
	} else {
		if client.Caps().Has(goimap.AuthCap("PLAIN")) {
			err = client.Authenticate(sasl.NewPlainClient("", account.Username, credential.Password))
		} else {
			err = client.Login(account.Username, credential.Password).Wait()
		}
	}
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	// RFC 2971 ID, sent post-authentication because that is where the servers
	// that care about it look for it. The Chinese providers advertise ID in the
	// greeting itself and treat an anonymous client as a worse citizen than an
	// identified one when deciding what to throttle; QQ and 163 both document
	// that third-party clients are expected to identify themselves. It is one
	// round trip on connect, so a provider that ignores ID loses nothing, and a
	// failure is deliberately non-fatal: ID is an optional courtesy and no
	// account should be unreachable because it was refused.
	if client.Caps().Has(goimap.CapID) {
		if _, idErr := client.ID(&goimap.IDData{
			Name:    "NexusMail",
			Version: version.Value,
			Vendor:  "NexusMail",
		}).Wait(); idErr != nil {
			slog.Debug("imap ID rejected", "account_id", account.ID, "error", idErr)
		}
	}
	guard.window.Store(int64(stall))
	return client, nil
}

// refreshMailboxCatalog lists the provider's mailboxes and upserts the
// classification. The LIST call is a one-shot round trip and the database writes
// are independent of any other IMAP state, so the sync callers deliberately run
// it *without* the command lock, keeping it off the new-mail path; it is also
// safe to call while holding the lock, which ensureArchiveMailbox does because it
// must not race another writer between LIST and CREATE. The sync path is expected
// to invoke it before syncAllMailboxes so the latter sees a complete catalog.
//
// It returns the LIST entries, because the mailbox attributes are not persisted
// and a caller that needs them — ensureArchiveMailbox looks for \Noselect
// containers — would otherwise have to LIST a second time.
func (s *Supervisor) refreshMailboxCatalog(ctx context.Context, rt *runtime, client *imapclient.Client) ([]*goimap.ListData, error) {
	items, err := client.List("", "*", nil).Collect()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	for _, item := range items {
		attrs := make([]string, len(item.Attrs))
		for i, attr := range item.Attrs {
			attrs[i] = string(attr)
		}
		role, mode := provider.ClassifyMailbox(item.Mailbox, attrs)
		var delimiter *string
		if item.Delim != 0 {
			value := string(item.Delim)
			delimiter = &value
		}
		mailbox := domain.Mailbox{AccountID: rt.account.ID, RemoteName: item.Mailbox, DisplayName: item.Mailbox, Delimiter: delimiter, Role: role, SyncMode: mode, CreatedAt: now, UpdatedAt: now}
		if err := s.repo.UpsertMailbox(ctx, &mailbox); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// syncAllMailboxes iterates the catalog and runs syncMailbox on each non-lazy
// mailbox, draining queued sync requests between them. The caller must hold
// the command lock; each syncMailbox re-selects its own mailbox, so this is
// only safe between mailboxes, never inside one.
func (s *Supervisor) syncAllMailboxes(ctx context.Context, rt *runtime, client *imapclient.Client) error {
	defer observe("sync_all", rt.account.ID)()
	mailboxes, err := s.repo.ListMailboxes(ctx, rt.account.ID)
	if err != nil {
		return err
	}
	for _, mailbox := range mailboxes {
		if mailbox.SyncMode == "lazy" {
			continue
		}
		// Sent, drafts and archive change rarely, but syncing them cost a SELECT and
		// a UID SEARCH each regardless, on the same connection new mail needs. STATUS
		// answers "did anything move" in one command, so a quiet mailbox is skipped
		// unless reconciliation is due — which is the only thing that can change
		// without UIDNEXT changing.
		if mailbox.Role != "inbox" && s.mailboxQuiet(client, mailbox) {
			continue
		}
		if err := s.syncMailbox(ctx, client, mailbox, false); err != nil {
			return fmt.Errorf("sync %s: %w", mailbox.RemoteName, err)
		}
		// A full sync can span minutes on large accounts. Serve queued inbox
		// signals between mailboxes so new mail is not stuck behind it.
		if err := s.drainPending(ctx, rt, client); err != nil {
			return err
		}
	}
	return nil
}

// mailboxQuiet reports whether a mailbox can be skipped this pass: nothing new
// arrived and its reconciliation is not yet due. A STATUS failure returns false so
// the caller falls back to the full path rather than silently skipping a mailbox.
func (s *Supervisor) mailboxQuiet(client *imapclient.Client, mailbox domain.Mailbox) bool {
	if mailbox.UIDNext == nil || mailbox.UIDValidity == 0 {
		return false
	}
	value, ok := s.lastReconcile.Load(mailbox.ID)
	last, valid := value.(time.Time)
	if !ok || !valid || time.Since(last) >= reconcileIntervalFor(mailbox.Role) {
		return false
	}
	status, err := client.Status(mailbox.RemoteName, &goimap.StatusOptions{UIDNext: true, UIDValidity: true}).Wait()
	if err != nil {
		slog.Debug("mailbox status probe failed", "mailbox_id", mailbox.ID, "error", err)
		return false
	}
	return status.UIDNext != 0 && uint32(status.UIDNext) == *mailbox.UIDNext && status.UIDValidity == mailbox.UIDValidity
}

// drainPending services queued sync requests. The caller must hold the command
// lock; each syncMailbox re-selects its own mailbox, so this is only safe
// between mailboxes, never inside one.
func (s *Supervisor) drainPending(ctx context.Context, rt *runtime, client *imapclient.Client) error {
	for {
		select {
		case mailboxID := <-rt.syncReq:
			if mailboxID == 0 {
				if err := s.syncRole(ctx, client, rt.account.ID, "inbox", false); err != nil {
					return err
				}
				continue
			}
			mailbox, err := s.repo.GetMailbox(ctx, mailboxID)
			if err != nil {
				return err
			}
			if mailbox.AccountID != rt.account.ID {
				continue
			}
			if err := s.syncMailbox(ctx, client, mailbox, false); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (s *Supervisor) syncRole(ctx context.Context, client *imapclient.Client, accountID int64, role string, skipReconcile bool) error {
	mailbox, err := s.repo.GetMailboxByRole(ctx, accountID, role)
	if err != nil {
		return err
	}
	return s.syncMailbox(ctx, client, mailbox, skipReconcile)
}

// incrementalUIDRange returns the UID range to search for mail newer than the
// stored cursor, and whether a search is worth sending at all.
//
// The upper bound comes from the UIDNEXT that SELECT just reported, which is the
// only bound that is both finite and true. The two alternatives are both known
// to break:
//
//   - the 0="*" sentinel produces "3355:*", and a server that resolves "*" to a
//     UID below the start normalises the reversed range per RFC 3501 — so an
//     up-to-date mailbox re-fetched its newest message on every 5-second probe.
//   - math.MaxUint32 produces "3355:4294967295", which QQ refuses with
//     "NO System busy!" on UID SEARCH while accepting SELECT on the same
//     connection. That reply is classified as a throttle, so the account parked
//     in backoff and stopped syncing entirely while looking merely rate-limited.
//
// When the cursor already covers UIDNEXT-1 there is nothing to ask about, and
// skipping the round trip is what keeps the 5-second probe cheap. A server that
// omits UIDNEXT leaves only the sentinel, which is still better than a bound
// that a provider rejects outright.
func incrementalUIDRange(lastUID uint32, uidNext goimap.UID) (goimap.UIDSet, bool) {
	var set goimap.UIDSet
	start := goimap.UID(lastUID) + 1
	if uidNext == 0 {
		set.AddRange(start, 0)
		return set, true
	}
	if uidNext <= start {
		return set, false
	}
	set.AddRange(start, uidNext-1)
	return set, true
}

// syncMailbox ingests new UIDs and, unless skipReconcile is set, repairs
// flag/expunge drift. The 5s inbox probe passes skipReconcile=true: the safety
// net's only job is to surface new mail quickly, and reconciliation on a large
// inbox holds the command connection for the time the probe is supposed to
// save. Drift is still caught by the 5-minute periodic sync, so the worst case
// is a 5-minute delay on flag changes or remote expunges — the same as before
// the safety net existed at all.
func (s *Supervisor) syncMailbox(ctx context.Context, client *imapclient.Client, mailbox domain.Mailbox, skipReconcile bool) error {
	defer observe("sync_mailbox", mailbox.AccountID, "mailbox", mailbox.RemoteName, "role", mailbox.Role)()
	// CONDSTORE is requested so SELECT reports HIGHESTMODSEQ and the cursor can
	// record it. Nothing reads it back yet — reconciliation cannot narrow on it
	// without losing the ability to tell an expunge from an unchanged flag, see
	// reconcileFlags — but it is the anchor any future QRESYNC path needs, and it is
	// only useful if it was being recorded all along.
	condStore := client.Caps().Has(goimap.CapCondStore)
	selected, err := client.Select(mailbox.RemoteName, &goimap.SelectOptions{ReadOnly: true, CondStore: condStore}).Wait()
	if err != nil {
		// Naming the command matters when a provider rejects only some of them:
		// a bare "sync INBOX: NO System busy!" cannot be told apart from a
		// failing search or fetch, which is the difference between "the account
		// is throttled" and "this one command is refused".
		return fmt.Errorf("select: %w", err)
	}
	if mailbox.UIDValidity != 0 && mailbox.UIDValidity != selected.UIDValidity {
		if err := s.repo.ResetMailbox(ctx, mailbox.ID, selected.UIDValidity); err != nil {
			return err
		}
		mailbox.LastUID = 0
	}
	mailbox.UIDValidity = selected.UIDValidity
	criteria := &goimap.SearchCriteria{}
	var uids []goimap.UID
	if mailbox.LastUID == 0 {
		criteria.Since = time.Now().AddDate(0, 0, -30)
		search, err := client.UIDSearch(criteria, nil).Wait()
		if err != nil {
			return fmt.Errorf("uid search: %w", err)
		}
		uids = search.AllUIDs()
	} else if set, search := incrementalUIDRange(mailbox.LastUID, selected.UIDNext); search {
		criteria.UID = []goimap.UIDSet{set}
		result, err := client.UIDSearch(criteria, nil).Wait()
		if err != nil {
			return fmt.Errorf("uid search: %w", err)
		}
		uids = result.AllUIDs()
	}
	lastUID := mailbox.LastUID
	// pending collects a chunk of built messages so the flush at the end of the
	// chunk can write them under one writeMu + one transaction. Without this
	// batching, a 100-message syncMailbox pass would pay 100 commits.
	var pending []pendingMessage
	for start := 0; start < len(uids); start += 100 {
		end := min(start+100, len(uids))
		fetchOptions := &goimap.FetchOptions{
			UID: true, Envelope: true, Flags: true, InternalDate: true, RFC822Size: true,
			BodyStructure: &goimap.FetchItemBodyStructure{Extended: true},
		}
		messages, err := client.Fetch(goimap.UIDSetNum(uids[start:end]...), fetchOptions).Collect()
		if err != nil {
			return fmt.Errorf("fetch envelopes: %w", err)
		}
		for _, fetched := range messages {
			if mailbox.Role == "drafts" {
				var raw []byte
				if fetched.RFC822Size > 0 && fetched.RFC822Size <= maxInlineDraftImportBytes {
					section := &goimap.FetchItemBodySection{Peek: true}
					bodies, fetchErr := client.Fetch(goimap.UIDSetNum(fetched.UID), &goimap.FetchOptions{UID: true, BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
					if fetchErr == nil && len(bodies) > 0 {
						raw = bodies[0].FindBodySection(section)
					}
				}
				changed, draftID, err := s.storeRemoteDraft(ctx, mailbox, fetched, raw)
				if err != nil {
					if ctx.Err() != nil {
						return err
					}
					// Failing the whole mailbox here would abort before the cursor
					// advances, so one unstorable draft would stall every sync for
					// the account, including the inbox. Skip just this message.
					slog.Error("remote draft import failed", "account_id", mailbox.AccountID, "mailbox_id", mailbox.ID, "uid", fetched.UID, "error", err)
					if uint32(fetched.UID) > lastUID {
						lastUID = uint32(fetched.UID)
					}
					continue
				}
				if changed {
					s.events.Publish(ports.Event{Type: "DRAFT_UPDATED", Data: map[string]any{"draft_id": draftID, "account_id": mailbox.AccountID, "remote": true}})
				}
				if uint32(fetched.UID) > lastUID {
					lastUID = uint32(fetched.UID)
				}
				continue
			}
			// Build a MessageInput and its attachments in memory, then commit the
			// whole chunk to the store at once. The previous shape paid one
			// writeMu and one commit per message; a 100-message sync paid 100
			// commits, and WAL fsync is the dominant cost on the new-mail path.
			input, attachments, err := s.buildFetchedMessage(mailbox, fetched)
			if err != nil {
				return err
			}
			pending = append(pending, pendingMessage{input: input, attachments: attachments, fetched: fetched, uid: uint32(fetched.UID)})
			if uint32(fetched.UID) > lastUID {
				lastUID = uint32(fetched.UID)
			}
		}
		if len(pending) > 0 {
			if err := s.flushPending(ctx, mailbox, pending); err != nil {
				return err
			}
			pending = pending[:0]
		}
	}
	// Appending new UIDs is only half of sync: flags set elsewhere and messages
	// deleted elsewhere are invisible to the pass above. Skip this on the 5s
	// inbox probe — see the skipReconcile comment on syncMailbox.
	if !skipReconcile {
		if err := s.reconcileMailbox(ctx, client, mailbox, selected); err != nil {
			// Reconciliation is a repair pass, not the ingest path. Failing the mailbox
			// here would also discard the new mail just stored and stop the cursor from
			// advancing, so the error is recorded and the sync still commits.
			slog.Warn("mailbox reconcile failed", "account_id", mailbox.AccountID, "mailbox_id", mailbox.ID, "error", err)
		}
	}
	uidNext := uint32(selected.UIDNext)
	highest := selected.HighestModSeq
	return s.repo.UpdateMailboxCursor(ctx, mailbox.ID, selected.UIDValidity, lastUID, &uidNext, &highest)
}

// reconcileMailbox brings local rows back in line with the provider for changes
// the append-only pass cannot observe: \Seen and \Flagged set in another client,
// and messages deleted or expunged elsewhere.
//
// The mailbox must already be selected by the caller, which also holds the
// command lock, so everything this does is charged to new-mail latency. It is
// driven from the UIDs stored locally: one chunked UID FETCH of flags over that
// list answers both questions at once, because a UID the provider no longer has
// is simply absent from the response.
//
// The earlier shape asked the provider instead — UID SEARCH ALL plus a flag FETCH
// over everything it returned — which made the cost scale with the remote mailbox
// rather than with what the app holds. On a Gmail "All Mail" or a long-lived QQ
// inbox that is a six-figure UID list and a multi-megabyte flag response pulled
// under the foreground lock every five minutes, and new mail waited behind it.
func (s *Supervisor) reconcileMailbox(ctx context.Context, client *imapclient.Client, mailbox domain.Mailbox, selected *goimap.SelectData) error {
	if mailbox.Role == "drafts" {
		// Drafts have their own reconciliation with conflict handling in
		// ReconcileRemoteDraft; flags and expunges there mean something else.
		return nil
	}
	if value, ok := s.lastReconcile.Load(mailbox.ID); ok {
		if last, valid := value.(time.Time); valid && time.Since(last) < reconcileIntervalFor(mailbox.Role) {
			return nil
		}
	}
	// A UIDVALIDITY change is handled inside syncMailbox (ResetMailbox + the
	// local mailbox.UIDValidity update on the same value-typed struct that
	// flows into this call), so by the time we reach here the validity has
	// already been equalised and there is nothing for an extra check to do.
	stored, err := s.repo.ListMailboxUIDs(ctx, mailbox.ID)
	if err != nil {
		return fmt.Errorf("list local uids: %w", err)
	}
	return s.reconcileMailboxWithUIDs(ctx, client, mailbox, selected, stored)
}

// reconcileMailboxWithUIDs is reconcileMailbox over an explicit snapshot of the
// local UIDs. Splitting it out keeps the snapshot visible as an input, because
// which UIDs were asked about is exactly what decides which ones may be deleted.
func (s *Supervisor) reconcileMailboxWithUIDs(ctx context.Context, client *imapclient.Client, mailbox domain.Mailbox, selected *goimap.SelectData, stored []uint32) error {
	if len(stored) == 0 {
		s.lastReconcile.Store(mailbox.ID, time.Now())
		return nil
	}
	defer observe("reconcile", mailbox.AccountID, "mailbox", mailbox.RemoteName, "local_uids", len(stored))()
	present, changed, err := s.reconcileFlags(ctx, client, mailbox, stored)
	if err != nil {
		return err
	}
	removed, err := s.repo.DeleteMailboxUIDs(ctx, mailbox.ID, staleUIDs(stored, present))
	if err != nil {
		return fmt.Errorf("drop expunged: %w", err)
	}
	s.lastReconcile.Store(mailbox.ID, time.Now())
	if removed == 0 && changed == 0 {
		return nil
	}
	// One aggregate event: a per-message burst would overrun the realtime hub's
	// buffer and disconnect the very client waiting for the correction.
	s.events.Publish(ports.Event{Type: "MESSAGE_UPDATED", Data: map[string]any{
		"bulk": true, "reconciled": changed, "removed": removed, "mailbox_id": mailbox.ID,
	}})
	return nil
}

// staleUIDs returns the UIDs that were asked about and did not come back, which
// is the only safe definition of "expunged" here: anything that arrived after the
// caller's snapshot is simply not part of the pass and must be left alone.
//
// The result is deliberately not pre-sized from len(stored)-len(present). A
// provider is free to echo UIDs that were never in the request — some servers
// answer a chunked UID FETCH with the whole mailbox — which makes that difference
// negative and panics the calling body worker with "makeslice: cap out of range".
func staleUIDs(stored, present []uint32) []uint32 {
	seen := make(map[uint32]struct{}, len(present))
	for _, uid := range present {
		seen[uid] = struct{}{}
	}
	stale := make([]uint32, 0, len(stored))
	for _, uid := range stored {
		if _, ok := seen[uid]; !ok {
			stale = append(stale, uid)
		}
	}
	return stale
}

// reconcileFlags fetches the provider's flags for the UIDs held locally and
// writes back the ones that differ. It returns the UIDs the provider still has,
// which is what the caller uses to find the expunged ones.
//
// CHANGEDSINCE is deliberately not used. It would shrink the response, but a UID
// omitted because its flags did not change is indistinguishable from one that was
// expunged, and treating the first as the second deletes mail that still exists.
// The request is already bounded by the local row count, so the full answer is
// affordable.
func (s *Supervisor) reconcileFlags(ctx context.Context, client *imapclient.Client, mailbox domain.Mailbox, stored []uint32) ([]uint32, int, error) {
	present := make([]uint32, 0, len(stored))
	total := 0
	options := &goimap.FetchOptions{UID: true, Flags: true}
	for start := 0; start < len(stored); start += reconcileFlagChunk {
		end := min(start+reconcileFlagChunk, len(stored))
		chunk := stored[start:end]
		set := make([]goimap.UID, len(chunk))
		for index, uid := range chunk {
			set[index] = goimap.UID(uid)
		}
		fetched, err := client.Fetch(goimap.UIDSetNum(set...), options).Collect()
		if err != nil {
			return nil, total, fmt.Errorf("fetch flags: %w", err)
		}
		states := make([]ports.RemoteFlagState, 0, len(fetched))
		for _, item := range fetched {
			values := make([]string, len(item.Flags))
			for index, flag := range item.Flags {
				values[index] = string(flag)
			}
			present = append(present, uint32(item.UID))
			states = append(states, ports.RemoteFlagState{
				UID:       uint32(item.UID),
				IsRead:    hasFlag(item.Flags, goimap.FlagSeen),
				IsStarred: hasFlag(item.Flags, goimap.FlagFlagged),
				Flags:     values,
			})
		}
		changed, err := s.repo.ReconcileMailboxFlags(ctx, mailbox.ID, states)
		if err != nil {
			return nil, total, fmt.Errorf("apply flags: %w", err)
		}
		total += changed
	}
	return present, total, nil
}

// isAuthFailure reports whether the provider rejected the credentials rather than
// failing for a reason retrying could fix.
func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	var status *goimap.Error
	if errors.As(err, &status) {
		switch status.Code {
		case goimap.ResponseCodeAuthenticationFailed, goimap.ResponseCodeAuthorizationFailed, goimap.ResponseCodeExpired, goimap.ResponseCodePrivacyRequired:
			return true
		}
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"authenticationfailed", "authorizationfailed", "invalid credentials",
		"invalid password", "login denied", "authentication failed", "auth failed",
		"password error", "refresh oauth token",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// isRateLimited reports whether the provider is throttling the account. These
// errors are retryable but on a longer schedule than the network-error ladder:
// a 1-second reconnect against QQ/163 just keeps the throttle engaged, so the
// caller pairs this with rateLimitBackoff instead of the 1s→5m ladder.
func isRateLimited(err error) bool {
	if err == nil {
		return false
	}
	var status *goimap.Error
	if errors.As(err, &status) {
		// ResponseCodeUnavailable is what the RFC calls "try later"; some
		// providers also surface rate limits as a tagged BYE with this code.
		if status.Code == goimap.ResponseCodeUnavailable {
			return true
		}
	}
	text := strings.ToLower(err.Error())
	// QQ: "NO System busy!" (no quoted keywords), 163: "BYE Too many
	// concurrent connections", Outlook: "Too many simultaneous connections",
	// Gmail: "Quota exceeded".
	markers := []string{
		"system busy", "too many", "rate limit", "rate exceeded",
		"quota exceeded", "try later", "try again later",
	}
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func (s *Supervisor) storeRemoteDraft(ctx context.Context, mailbox domain.Mailbox, fetched *imapclient.FetchMessageBuffer, raw []byte) (bool, int64, error) {
	if fetched.Envelope == nil {
		return false, 0, nil
	}
	parsed := mailparser.Parsed{}
	if len(raw) > 0 {
		value, err := mailparser.Parse(bytes.NewReader(raw))
		if err != nil {
			return false, 0, err
		}
		parsed = value
	}
	rfcID := strings.TrimSpace(fetched.Envelope.MessageID)
	if rfcID == "" {
		rfcID = fmt.Sprintf("<remote-%d-%d-%d-%d@nexusmail.local>", mailbox.AccountID, mailbox.ID, mailbox.UIDValidity, fetched.UID)
	}
	to := parsed.To
	cc := parsed.CC
	bcc := parsed.BCC
	if len(to) == 0 {
		to = imapAddresses(fetched.Envelope.To)
	}
	if len(cc) == 0 {
		cc = imapAddresses(fetched.Envelope.Cc)
	}
	remoteTime := fetched.InternalDate.UnixMilli()
	if remoteTime <= 0 {
		remoteTime = time.Now().UnixMilli()
	}
	mailboxID, validity, uid := mailbox.ID, mailbox.UIDValidity, uint32(fetched.UID)
	draft := domain.Draft{
		AccountID: mailbox.AccountID, RFCMessageID: rfcID, Revision: 1,
		ToJSON: encodeMailAddresses(to), CCJSON: encodeMailAddresses(cc), BCCJSON: encodeMailAddresses(bcc),
		Subject: fetched.Envelope.Subject, BodyText: parsed.Text, Status: "draft", RemoteSyncState: "synced",
		RemoteMailboxID: &mailboxID, RemoteUIDValidity: &validity, RemoteUID: &uid, RemoteUpdatedAt: &remoteTime,
		CreatedAt: remoteTime, UpdatedAt: remoteTime,
	}
	if parsed.Subject != "" {
		draft.Subject = parsed.Subject
	}
	reconciled, changed, err := s.repo.ReconcileRemoteDraft(ctx, &draft)
	return changed, reconciled.ID, err
}

func imapAddresses(input []goimap.Address) []*mail.Address {
	result := make([]*mail.Address, 0, len(input))
	for _, value := range input {
		if value.Addr() != "" {
			result = append(result, &mail.Address{Name: value.Name, Address: value.Addr()})
		}
	}
	return result
}

func encodeMailAddresses(input []*mail.Address) string {
	values := make([]string, 0, len(input))
	for _, value := range input {
		values = append(values, formatAddress(value.Name, value.Address))
	}
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

// specialsInDisplayName are the RFC 5322 "specials" that force a display name to
// be quoted. A bare UTF-8 name needs no quoting, which is what keeps the stored
// form readable.
const specialsInDisplayName = `()<>[]:;@\,."`

// formatAddress renders "Name <addr>" without RFC 2047 encoding. net/mail's
// Address.String() cannot be used here: it re-encodes every non-ASCII display
// name into an encoded-word, so a name go-imap had already decoded came back out
// as a literal "=?utf-8?q?...?=" — visible in the UI and indexed that way by
// FTS5, which also made the readable name unsearchable. Quoting still follows
// RFC 5322 so the result round-trips through mail.ParseAddress when a draft
// built from these values is sent.
func formatAddress(name, address string) string {
	// Whitespace is collapsed to single spaces. A CR or LF here is either a folded
	// header the decoder left behind or an injection attempt; it has to go, but as a
	// space rather than by deletion, so the two sides of the break do not fuse into
	// one word. This value is parsed back and written into an outgoing draft.
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return address
	}
	if strings.ContainsAny(name, specialsInDisplayName) {
		var quoted strings.Builder
		quoted.Grow(len(name) + 2)
		quoted.WriteByte('"')
		for _, r := range name {
			if r == '"' || r == '\\' {
				quoted.WriteByte('\\')
			}
			quoted.WriteRune(r)
		}
		quoted.WriteByte('"')
		name = quoted.String()
	}
	return name + " <" + address + ">"
}

// pendingMessage is one row ready to be flushed by the supervisor at the end
// of a UID chunk. input is the message + mailbox mapping; attachments are
// kept here so the batch can persist them with the same writeMu section.
type pendingMessage struct {
	input       ports.MessageInput
	attachments []domain.Attachment
	fetched     *imapclient.FetchMessageBuffer
	uid         uint32
}

// buildFetchedMessage turns an IMAP FetchMessageBuffer into the shape the
// repository ingests. It does no I/O: the supervisor collects a chunk and
// flushes it under one transaction.
func (s *Supervisor) buildFetchedMessage(mailbox domain.Mailbox, fetched *imapclient.FetchMessageBuffer) (ports.MessageInput, []domain.Attachment, error) {
	if fetched.Envelope == nil {
		return ports.MessageInput{}, nil, errors.New("fetched message has no envelope")
	}
	envelope := fetched.Envelope
	rfcID := strings.TrimSpace(envelope.MessageID)
	dedupeSource := rfcID + "\x00" + strconv.FormatInt(fetched.RFC822Size, 10)
	if rfcID == "" {
		dedupeSource = fmt.Sprintf("%d:%d:%d:%d", mailbox.ID, mailbox.UIDValidity, fetched.UID, fetched.RFC822Size)
	}
	digest := sha256.Sum256([]byte(dedupeSource))
	from := addresses(envelope.From)
	to := addresses(envelope.To)
	cc := addresses(envelope.Cc)
	fromJSON, _ := json.Marshal(from)
	toJSON, _ := json.Marshal(to)
	ccJSON, _ := json.Marshal(cc)
	now := time.Now().UnixMilli()
	received := fetched.InternalDate.UnixMilli()
	if received == 0 {
		received = now
	}
	message := domain.Message{
		AccountID: mailbox.AccountID, Direction: "incoming", DedupeKey: digest[:], Subject: envelope.Subject,
		Sender: strings.Join(from, " "), Recipients: strings.Join(append(to, cc...), " "), FromJSON: string(fromJSON), ToJSON: string(toJSON), CCJSON: string(ccJSON),
		BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]", BodyState: "metadata", SizeBytes: fetched.RFC822Size,
		ReceivedAt: received, IsRead: hasFlag(fetched.Flags, goimap.FlagSeen), IsStarred: hasFlag(fetched.Flags, goimap.FlagFlagged), CreatedAt: now, UpdatedAt: now,
	}
	if rfcID != "" {
		message.RFCMessageID = &rfcID
	}
	if !envelope.Date.IsZero() {
		value := envelope.Date.UnixMilli()
		message.SentAt = &value
	}
	flagValues := make([]string, len(fetched.Flags))
	for i, flag := range fetched.Flags {
		flagValues[i] = string(flag)
	}
	var attachments []domain.Attachment
	if fetched.BodyStructure != nil {
		fetched.BodyStructure.Walk(func(path []int, part goimap.BodyStructure) bool {
			single, ok := part.(*goimap.BodyStructureSinglePart)
			if !ok {
				return true
			}
			filename := single.Filename()
			disposition := single.Disposition()
			if filename == "" && disposition == nil {
				return true
			}
			dispositionValue := "attachment"
			if disposition != nil && strings.EqualFold(disposition.Value, "inline") {
				dispositionValue = "inline"
			}
			partValues := make([]string, len(path))
			for i, value := range path {
				partValues[i] = strconv.Itoa(value)
			}
			att := domain.Attachment{PartID: strings.Join(partValues, "."), Filename: filename, ContentType: single.MediaType(), Disposition: dispositionValue, SizeBytes: int64(single.Size), FetchState: "metadata", CreatedAt: now, UpdatedAt: now}
			if single.ID != "" {
				value := single.ID
				att.ContentID = &value
			}
			attachments = append(attachments, att)
			return true
		})
	}
	return ports.MessageInput{
		Message:      &message,
		MailboxID:    mailbox.ID,
		UID:          uint32(fetched.UID),
		Flags:        flagValues,
		InternalDate: fetched.InternalDate,
	}, attachments, nil
}

// flushPending commits a chunk of built messages under a single writeMu and
// publishes a NEW_EMAIL event for each newly created row. Attachments are
// written in the same single-transaction section; MessageID on each
// attachment is patched to the id the repository assigned.
func (s *Supervisor) flushPending(ctx context.Context, mailbox domain.Mailbox, pending []pendingMessage) error {
	inputs := make([]ports.MessageInput, len(pending))
	for i, item := range pending {
		inputs[i] = item.input
	}
	ids, created, err := s.repo.BatchCreateOrUpdateMessages(ctx, inputs)
	if err != nil {
		return err
	}
	// Patch attachment rows with the now-known message ids, then batch upsert.
	var attachments []domain.Attachment
	for i, item := range pending {
		if len(item.attachments) == 0 {
			continue
		}
		for j := range item.attachments {
			item.attachments[j].MessageID = ids[i]
		}
		attachments = append(attachments, item.attachments...)
	}
	if len(attachments) > 0 {
		if err := s.repo.BatchUpsertAttachments(ctx, attachments); err != nil {
			return err
		}
	}
	for i, item := range pending {
		if !created[i] {
			continue
		}
		data := map[string]any{"message_id": ids[i], "account_id": mailbox.AccountID, "mailbox_id": mailbox.ID}
		// The body is still empty at this point, so only the subject can be
		// scanned. It catches the common "【服务】验证码 123456" shape without
		// waiting for the prefetch; fetchBody re-runs detection on the full
		// text and replaces this notification.
		if item.fetched.Envelope != nil && withinOTPWindow(item.fetched.InternalDate) {
			if code, ok := mailparser.DetectOTP(item.fetched.Envelope.Subject, "", ""); ok {
				data["otp_code"] = code
				data["otp_subject"] = item.fetched.Envelope.Subject
			}
		}
		s.events.Publish(ports.Event{Type: "NEW_EMAIL", Data: data})
	}
	return nil
}

func addresses(input []goimap.Address) []string {
	result := make([]string, 0, len(input))
	for _, value := range input {
		if value.Addr() != "" {
			result = append(result, formatAddress(value.Name, value.Addr()))
		}
	}
	return result
}
func hasFlag(flags []goimap.Flag, target goimap.Flag) bool {
	for _, flag := range flags {
		if flag == target {
			return true
		}
	}
	return false
}
func (s *Supervisor) requestSync(rt *runtime) {
	select {
	case rt.syncReq <- 0:
	default:
	}
}

// requestSyncCooldown is the minimum interval between two RequestMailbox
// signals for the same mailbox. The 5s probe and the IDLE loop are the
// authoritative source of new-mail detection; this channel only exists to
// express "the user just opened this folder, prefer not to wait the next
// probe tick", and a passive reader must not be able to amplify into
// sustained IMAP traffic.
const requestSyncCooldown = 5 * time.Second

func (s *Supervisor) RequestMailbox(ctx context.Context, mailboxID int64) error {
	mailbox, err := s.repo.GetMailbox(ctx, mailboxID)
	if err != nil {
		return err
	}
	rt, err := s.runtime(mailbox.AccountID)
	if err != nil {
		return err
	}
	now := time.Now()
	if prev, ok := s.lastSyncReq.Load(mailboxID); ok {
		if now.Sub(prev.(time.Time)) < requestSyncCooldown {
			return nil
		}
	}
	s.lastSyncReq.Store(mailboxID, now)
	select {
	case rt.syncReq <- mailboxID:
	default:
	}
	return nil
}
func (s *Supervisor) closeCommand(rt *runtime, client *imapclient.Client) {
	rt.client.CompareAndSwap(client, nil)
	_ = client.Close()
}
func (s *Supervisor) setError(ctx context.Context, id int64, status string, err error) {
	// A rate-limit error is self-healing: the loop waits rateLimitBackoff and
	// retries on its own. Storing the raw "imap: NO System busy!" string in
	// the account row would replay the same message to the user every time
	// the loop retries, which is exactly the noise we are trying to avoid.
	// The amber "backoff" dot already signals "trying, just slowly", so we
	// clear last_error instead. The full error still goes to slog and the
	// realtime event for operators and tools that need it.
	var stored *string
	if !isRateLimited(err) {
		text := err.Error()
		stored = &text
	}
	if updateErr := s.repo.UpdateAccountStatus(ctx, id, status, stored); updateErr != nil {
		slog.Error("mail account status update failed", "account_id", id, "status", status, "error", updateErr)
	}
	slog.Error("mail account sync failed", "account_id", id, "status", status, "error", err.Error())
	s.events.Publish(ports.Event{Type: "ACCOUNT_STATUS", Data: map[string]any{"account_id": id, "status": status, "error": err.Error()}})
}

// backoffDelay jitters a retry delay. Half of the delay is a hard floor and the
// rest is random: full jitter over [0, delay] spread the load nicely but made
// the long windows meaningless, because a 15-minute rateLimitBackoff drawing 8
// seconds reconnects inside the throttle window it was waiting out and re-arms
// it. Every such draw resets the provider's clock, which is how an account
// stayed amber for hours despite a ladder that on paper backs off to minutes.
// Equal jitter keeps enough spread to avoid synchronising every account on one
// instant after a shared outage while making the floor worth its name.
func backoffDelay(delay time.Duration) time.Duration {
	if delay <= 0 {
		return 0
	}
	floor := delay / 2
	span := delay - floor
	// Never return 0 for a positive delay: a zero-length timer is a spin.
	return max(floor+time.Duration(rand.Int64N(int64(span)+1)), 1)
}

// retryDelay maps a failure to how long the caller must leave the provider
// alone. Credentials the server already refused and a throttle it has already
// engaged both need a window far longer than the network ladder, and picking
// the ladder for either is what turns one rejection into a sustained hammering.
func retryDelay(err error, ladder time.Duration) time.Duration {
	switch {
	case isAuthFailure(err):
		return authBackoff
	case isRateLimited(err):
		return rateLimitBackoff
	}
	return ladder
}

func waitBackoff(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(backoffDelay(delay))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// FetchBody retrieves a message body for a waiting caller and takes priority
// over background prefetch.
func (s *Supervisor) FetchBody(ctx context.Context, messageID int64) error {
	_, err := s.fetchBody(ctx, messageID, false)
	return err
}

// otpNotice carries what a verification-code notification needs. The subject
// travels with the code so the browser can say which service the code is for
// without another round-trip.
type otpNotice struct {
	Code    string
	Subject string
}

// fetchBody returns any verification code the body carries instead of publishing
// it, because this runs while holding the account's command connection and the
// new-mail latency budget leaves no room for extra work under that lock.
func (s *Supervisor) fetchBody(ctx context.Context, messageID int64, background bool) (otp otpNotice, resultErr error) {
	select {
	case s.bodySlots <- struct{}{}:
		defer func() { <-s.bodySlots }()
	case <-ctx.Done():
		return otpNotice{}, ctx.Err()
	}
	message, _, err := s.repo.GetMessage(ctx, messageID)
	if err != nil {
		return otpNotice{}, err
	}
	if message.BodyState == "ready" {
		return otpNotice{}, nil
	}
	if err := s.repo.SetMessageBodyState(ctx, messageID, "fetching"); err != nil {
		return otpNotice{}, err
	}
	defer func() {
		if resultErr != nil {
			_ = s.repo.SetMessageBodyState(context.Background(), messageID, "error")
		}
	}()
	location, err := s.repo.MessageLocation(ctx, messageID)
	if err != nil {
		return otpNotice{}, err
	}
	rt, err := s.runtime(location.Account.ID)
	if err != nil {
		return otpNotice{}, err
	}
	if background {
		if !rt.lockBackground(ctx) {
			return otpNotice{}, ctx.Err()
		}
	} else {
		rt.lock()
	}
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return otpNotice{}, ports.Unavailablef("account is offline")
	}
	if _, err := client.Select(location.Mailbox.RemoteName, &goimap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return otpNotice{}, err
	}
	section := &goimap.FetchItemBodySection{Peek: true}
	items, err := client.Fetch(goimap.UIDSetNum(goimap.UID(location.UID)), &goimap.FetchOptions{UID: true, BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
	if err != nil || len(items) == 0 {
		if err == nil {
			err = errors.New("message body not found")
		}
		return otpNotice{}, err
	}
	body := items[0].FindBodySection(section)
	parsed, err := mailparser.Parse(bytes.NewReader(body))
	if err != nil {
		return otpNotice{}, err
	}
	blob, err := s.blobs.Put(ctx, bytes.NewReader(body), "cache")
	if err != nil {
		return otpNotice{}, err
	}
	if err := s.repo.UpdateMessageBody(ctx, messageID, parsed.Text, parsed.HTML, parsed.Snippet, &blob.ID); err != nil {
		return otpNotice{}, err
	}
	if !withinOTPWindow(time.UnixMilli(message.ReceivedAt)) {
		return otpNotice{}, nil
	}
	code, ok := mailparser.DetectOTP(message.Subject, parsed.Text, parsed.HTML)
	if !ok {
		return otpNotice{}, nil
	}
	return otpNotice{Code: code, Subject: message.Subject}, nil
}

// withinOTPWindow reports whether a message is recent enough that surfacing its
// verification code as a notification is still useful.
func withinOTPWindow(received time.Time) bool {
	return !received.IsZero() && time.Since(received) <= otpFreshness
}

func (s *Supervisor) enqueueBodyCandidates(ctx context.Context, accountID int64) {
	ids, err := s.repo.ListBodyCandidateIDs(ctx, accountID, maxInlineDraftImportBytes, 100)
	if err != nil {
		return
	}
	// Pre-filter: the candidate query cannot exclude 'error' or already-seen
	// rows without a schema change, and the bodyAttempts cap is enforced here
	// so a body that has already failed maxBodyAttempts times is not retried
	// for the life of the process. Only the survivors are flipped to 'queued'
	// in one batch write instead of N single-row UPDATEs.
	candidates := make([]int64, 0, len(ids))
	for _, id := range ids {
		if value, ok := s.bodyAttempts.Load(id); ok {
			if attempts, valid := value.(int); valid && attempts >= maxBodyAttempts {
				continue
			}
		}
		if _, loaded := s.bodySeen.LoadOrStore(id, struct{}{}); loaded {
			continue
		}
		candidates = append(candidates, id)
	}
	if len(candidates) == 0 {
		return
	}
	if err := s.repo.BatchSetMessageBodyState(ctx, candidates, "queued"); err != nil {
		for _, id := range candidates {
			s.bodySeen.Delete(id)
		}
		return
	}
	for _, id := range candidates {
		select {
		case s.bodyQueue <- id:
		case <-ctx.Done():
			s.bodySeen.Delete(id)
			_ = s.repo.SetMessageBodyState(context.Background(), id, "metadata")
			return
		default:
			s.bodySeen.Delete(id)
			_ = s.repo.SetMessageBodyState(context.Background(), id, "metadata")
			return
		}
	}
}

func (s *Supervisor) bodyWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.bodyQueue:
			fetchCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			otp, err := s.fetchBody(fetchCtx, id, true)
			cancel()
			s.bodySeen.Delete(id)
			s.recordBodyAttempt(id, err)
			if err == nil {
				// Published after fetchBody released the command lock. The code is
				// carried on the event so the browser can raise a copyable
				// notification without another round-trip.
				data := map[string]any{"message_id": id}
				if otp.Code != "" {
					data["otp_code"] = otp.Code
					data["otp_subject"] = otp.Subject
				}
				s.events.Publish(ports.Event{Type: "MESSAGE_UPDATED", Data: data})
			} else if !errors.Is(err, context.Canceled) {
				// Step aside briefly on failure so a flaky provider does not eat
				// every body slot in a tight loop. maxBodyAttempts already caps the
				// total attempts per message, this just spreads them out.
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
		}
	}
}

// recordBodyAttempt maintains the per-message failure count the prefetch cap
// reads. A success clears the entry so a message that recovers is not held
// against its earlier failures. Cancellation is not a failure of the message: the
// process is shutting down or the caller went away.
func (s *Supervisor) recordBodyAttempt(id int64, err error) {
	if err == nil {
		s.bodyAttempts.Delete(id)
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	attempts := 0
	if value, ok := s.bodyAttempts.Load(id); ok {
		if current, valid := value.(int); valid {
			attempts = current
		}
	}
	s.bodyAttempts.Store(id, attempts+1)
}

func (s *Supervisor) FetchAttachment(ctx context.Context, messageID, attachmentID int64) (domain.BlobObject, domain.Attachment, error) {
	attachment, err := s.repo.GetAttachment(ctx, messageID, attachmentID)
	if err != nil {
		return domain.BlobObject{}, attachment, err
	}
	if attachment.BlobID != nil {
		blob, err := s.repo.GetBlob(ctx, *attachment.BlobID)
		return blob, attachment, err
	}
	location, err := s.repo.MessageLocation(ctx, messageID)
	if err != nil {
		return domain.BlobObject{}, attachment, err
	}
	rt, err := s.runtime(location.Account.ID)
	if err != nil {
		return domain.BlobObject{}, attachment, err
	}
	path, err := parsePartID(attachment.PartID)
	if err != nil {
		return domain.BlobObject{}, attachment, err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return domain.BlobObject{}, attachment, ports.Unavailablef("account is offline")
	}
	if _, err := client.Select(location.Mailbox.RemoteName, &goimap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return domain.BlobObject{}, attachment, err
	}
	section := &goimap.FetchItemBodySection{Part: path, Peek: true}
	items, err := client.Fetch(goimap.UIDSetNum(goimap.UID(location.UID)), &goimap.FetchOptions{UID: true, BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
	if err != nil || len(items) == 0 {
		if err == nil {
			err = ports.NotFoundf("attachment not found")
		}
		return domain.BlobObject{}, attachment, err
	}
	blob, err := s.blobs.Put(ctx, bytes.NewReader(items[0].FindBodySection(section)), "cache")
	if err != nil {
		return domain.BlobObject{}, attachment, err
	}
	if err := s.repo.UpdateAttachmentBlob(ctx, attachment.ID, blob.ID); err != nil {
		return domain.BlobObject{}, attachment, err
	}
	attachment.BlobID = &blob.ID
	attachment.FetchState = "ready"
	return blob, attachment, nil
}

func (s *Supervisor) SetFlags(ctx context.Context, messageID int64, isRead, isStarred *bool) error {
	location, err := s.repo.MessageLocation(ctx, messageID)
	if err != nil {
		return err
	}
	rt, err := s.runtime(location.Account.ID)
	if err != nil {
		return err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return ports.Unavailablef("account is offline")
	}
	if _, err := client.Select(location.Mailbox.RemoteName, nil).Wait(); err != nil {
		return err
	}
	uidSet := goimap.UIDSetNum(goimap.UID(location.UID))
	// Ordered on purpose: a map range would emit the two STOREs in a random order,
	// so a provider that only reports the last one, or a test asserting the wire
	// sequence, would see different behaviour run to run.
	for _, update := range []struct {
		flag  goimap.Flag
		value *bool
	}{
		{goimap.FlagSeen, isRead},
		{goimap.FlagFlagged, isStarred},
	} {
		flag, value := update.flag, update.value
		if value == nil {
			continue
		}
		op := goimap.StoreFlagsDel
		if *value {
			op = goimap.StoreFlagsAdd
		}
		if _, err := client.Store(uidSet, &goimap.StoreFlags{Op: op, Silent: true, Flags: []goimap.Flag{flag}}, nil).Collect(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Supervisor) Archive(ctx context.Context, messageID int64) error {
	location, err := s.repo.MessageLocation(ctx, messageID)
	if err != nil {
		return err
	}
	rt, err := s.runtime(location.Account.ID)
	if err != nil {
		return err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return ports.Unavailablef("account is offline")
	}
	destination, err := s.ensureArchiveMailbox(ctx, rt, client)
	if err != nil {
		return err
	}
	if destination.ID == location.Mailbox.ID {
		return nil
	}
	if _, err := client.Select(location.Mailbox.RemoteName, nil).Wait(); err != nil {
		return err
	}
	uidSet := goimap.UIDSetNum(goimap.UID(location.UID))
	if client.Caps().Has(goimap.CapMove) {
		data, moveErr := client.Move(uidSet, destination.RemoteName).Wait()
		if moveErr != nil {
			return moveErr
		}
		return s.repo.MoveMessageLocation(ctx, messageID, location.Mailbox.ID, destination.ID, firstDestinationUID(data.DestUIDs))
	}
	// COPY + \Deleted + expunge is the fallback. The expunge is not optional: the
	// local row is dropped from the source mailbox either way, so a message left
	// behind on the server is one the user archived here and still sees in the
	// provider's own web client. QQ advertises neither MOVE nor UIDPLUS on some
	// connections, which is exactly the path that has to remove it.
	expungeSafe, err := noPendingDeletes(client)
	if err != nil {
		return err
	}
	copyData, err := client.Copy(uidSet, destination.RemoteName).Wait()
	if err != nil {
		return err
	}
	if _, err = client.Store(uidSet, &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Collect(); err != nil {
		return err
	}
	switch {
	case client.Caps().Has(goimap.CapUIDPlus):
		if _, err = client.UIDExpunge(uidSet).Collect(); err != nil {
			return err
		}
	case expungeSafe:
		// Plain EXPUNGE removes every \Deleted message in the mailbox, so it is only
		// safe when this message is the only one carrying the flag. Another client's
		// pending deletes must not be finalised as a side effect of archiving.
		if _, err = client.Expunge().Collect(); err != nil {
			return err
		}
	default:
		slog.Warn("archive left message on server: mailbox has other \\Deleted messages and provider lacks UIDPLUS",
			"account_id", location.Account.ID, "mailbox", location.Mailbox.RemoteName, "uid", location.UID)
	}
	var destinationUID *uint32
	if copyData != nil {
		destinationUID = firstDestinationUID(copyData.DestUIDs)
	}
	return s.repo.MoveMessageLocation(ctx, messageID, location.Mailbox.ID, destination.ID, destinationUID)
}

// noPendingDeletes reports whether the selected mailbox currently holds no
// message flagged \Deleted, which is the precondition for a plain EXPUNGE being
// equivalent to expunging one UID.
//
// It is deliberately conservative: only a search that came back and was empty
// answers true. An error, or a response whose set cannot be read as UIDs, answers
// false, because losing another client's pending deletes is worse than leaving one
// archived message on the server.
func noPendingDeletes(client *imapclient.Client) (bool, error) {
	data, err := client.UIDSearch(&goimap.SearchCriteria{Flag: []goimap.Flag{goimap.FlagDeleted}}, nil).Wait()
	if err != nil {
		return false, err
	}
	if data.All == nil {
		return true, nil
	}
	set, ok := data.All.(goimap.UIDSet)
	if !ok {
		return false, nil
	}
	// A dynamic set ("*") is a server bug that AllUIDs would panic on; treat it as
	// unknown rather than empty.
	uids, ok := set.Nums()
	if !ok {
		return false, nil
	}
	return len(uids) == 0, nil
}

// archiveCandidateNames are the mailbox names tried when creating an archive
// folder, in order. Each must classify as the archive role via
// provider.ClassifyMailbox, otherwise the folder would be created and then not
// found. The plain root-level name comes first because that is where a provider
// which allows it puts a real sibling of INBOX.
var archiveCandidateNames = []string{"Archive", "Archives"}

// ensureArchiveMailbox returns the account's archive mailbox, creating one on the
// provider when none exists.
//
// QQ and 163 ship no archive folder and advertise no \Archive special-use
// attribute, so the role was simply absent and every archive attempt failed with
// "archive mailbox is unavailable". Creating the folder is what makes the action
// mean something on those providers, and it is done once: the second call finds
// the role and returns immediately.
//
// The caller must hold the command lock — this issues CREATE and LIST on the
// command connection.
func (s *Supervisor) ensureArchiveMailbox(ctx context.Context, rt *runtime, client *imapclient.Client) (domain.Mailbox, error) {
	if mailbox, err := s.repo.GetMailboxByRole(ctx, rt.account.ID, "archive"); err == nil {
		return mailbox, nil
	}
	// The catalog may predate a folder created in another client. Re-list before
	// concluding the account has no archive at all.
	items, err := s.refreshMailboxCatalog(ctx, rt, client)
	if err != nil {
		return domain.Mailbox{}, err
	}
	if mailbox, err := s.repo.GetMailboxByRole(ctx, rt.account.ID, "archive"); err == nil {
		return mailbox, nil
	}
	var createErrs []error
	for _, name := range archiveCandidateNames {
		for _, candidate := range archivePaths(name, items) {
			if err := client.Create(candidate, archiveCreateOptions(client)).Wait(); err != nil {
				createErrs = append(createErrs, fmt.Errorf("create %q: %w", candidate, err))
				continue
			}
			slog.Info("created archive mailbox", "account_id", rt.account.ID, "mailbox", candidate)
			// Trust LIST, not the name that was requested: a provider is free to
			// place the folder somewhere else, and the stored remote name has to be
			// the one SELECT and COPY will accept.
			if _, err := s.refreshMailboxCatalog(ctx, rt, client); err != nil {
				return domain.Mailbox{}, err
			}
			if mailbox, err := s.repo.GetMailboxByRole(ctx, rt.account.ID, "archive"); err == nil {
				return mailbox, nil
			}
			return domain.Mailbox{}, fmt.Errorf("created archive mailbox %q but it did not classify as archive", candidate)
		}
	}
	if len(createErrs) > 0 {
		return domain.Mailbox{}, ports.Unavailablef("archive mailbox is unavailable: %w", errors.Join(createErrs...))
	}
	return domain.Mailbox{}, ports.Unavailablef("archive mailbox is unavailable")
}

// archiveCreateOptions asks for the \Archive special-use attribute when the
// provider supports CREATE-SPECIAL-USE, so a server that understands roles
// records this folder as the archive for every other client too. QQ and 163 do
// not advertise it and get a plain CREATE.
func archiveCreateOptions(client *imapclient.Client) *goimap.CreateOptions {
	if !client.Caps().Has(goimap.Cap("CREATE-SPECIAL-USE")) {
		return nil
	}
	return &goimap.CreateOptions{SpecialUse: []goimap.MailboxAttr{goimap.MailboxAttrArchive}}
}

// archivePaths returns where to try creating an archive folder called name: at
// the root first, then under each \Noselect container the provider exposes.
//
// QQ accepts both, but a provider that only allows user folders beneath a
// container ("其他文件夹" on QQ, "[Gmail]" on Gmail) rejects the root-level CREATE,
// and the nested path is the one that works there. \Noselect is the server's own
// statement that a folder holds only children, which is why the attribute is used
// rather than guessing from the stored role — a top-level folder the user created
// for their own mail must never become the archive's parent.
func archivePaths(name string, items []*goimap.ListData) []string {
	paths := []string{name}
	for _, item := range items {
		if item.Delim == 0 {
			continue
		}
		noselect := false
		for _, attr := range item.Attrs {
			if attr == goimap.MailboxAttrNoSelect {
				noselect = true
				break
			}
		}
		if !noselect {
			continue
		}
		paths = append(paths, item.Mailbox+string(item.Delim)+name)
	}
	return paths
}

// SetSeenBulk adds \Seen to many messages, grouped so each account's command
// connection is taken once. It returns the message IDs the provider accepted so
// the caller only writes through the rows that really changed remotely.
//
// Failures are per account: one offline mailbox must not discard the flags that
// other accounts already stored.
func (s *Supervisor) SetSeenBulk(ctx context.Context, messageIDs []int64) ([]int64, error) {
	locations, err := s.repo.MessageLocations(ctx, messageIDs)
	if err != nil {
		return nil, err
	}
	groups := make(map[int64]map[int64][]ports.MessageLocation)
	for _, location := range locations {
		byMailbox := groups[location.Account.ID]
		if byMailbox == nil {
			byMailbox = make(map[int64][]ports.MessageLocation)
			groups[location.Account.ID] = byMailbox
		}
		byMailbox[location.Mailbox.ID] = append(byMailbox[location.Mailbox.ID], location)
	}
	done := make([]int64, 0, len(locations))
	var failures []error
	for accountID, byMailbox := range groups {
		if ctx.Err() != nil {
			return done, ctx.Err()
		}
		updated, err := s.setSeenAccount(ctx, accountID, byMailbox)
		done = append(done, updated...)
		if err != nil {
			failures = append(failures, fmt.Errorf("account %d: %w", accountID, err))
		}
	}
	return done, errors.Join(failures...)
}

// setSeenAccount holds one account's command lock for the duration of one
// mailbox at a time, not for the whole account. The 5s inbox probe and the
// IDLE-driven sync contend for the same lock, so a slow mailbox (large chunk,
// network latency) cannot block the new-mail path for the rest of the
// account. The lock is released and re-taken per mailbox; the IMAP connection
// itself is shared and stays put.
func (s *Supervisor) setSeenAccount(ctx context.Context, accountID int64, byMailbox map[int64][]ports.MessageLocation) ([]int64, error) {
	rt, err := s.runtime(accountID)
	if err != nil {
		return nil, err
	}
	done := make([]int64, 0)
	var failures []error
	for _, group := range byMailbox {
		if ctx.Err() != nil {
			return done, ctx.Err()
		}
		rt.lock()
		client := rt.client.Load()
		if client == nil {
			rt.unlock()
			return nil, ports.Unavailablef("account is offline")
		}
		if _, err := client.Select(group[0].Mailbox.RemoteName, nil).Wait(); err != nil {
			failures = append(failures, fmt.Errorf("select %q: %w", group[0].Mailbox.RemoteName, err))
			rt.unlock()
			continue
		}
		for start := 0; start < len(group); start += bulkFlagChunk {
			end := min(start+bulkFlagChunk, len(group))
			chunk := group[start:end]
			uids := make([]goimap.UID, len(chunk))
			for index, location := range chunk {
				uids[index] = goimap.UID(location.UID)
			}
			store := &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagSeen}}
			if _, err := client.Store(goimap.UIDSetNum(uids...), store, nil).Collect(); err != nil {
				failures = append(failures, err)
				continue
			}
			for _, location := range chunk {
				done = append(done, location.MessageID)
			}
		}
		rt.unlock()
	}
	return done, errors.Join(failures...)
}

func firstDestinationUID(set goimap.NumSet) *uint32 {
	uids, ok := set.(goimap.UIDSet)
	if !ok {
		return nil
	}
	values, ok := uids.Nums()
	if !ok || len(values) == 0 {
		return nil
	}
	value := uint32(values[0])
	return &value
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
func parsePartID(value string) ([]int, error) {
	parts := strings.Split(value, ".")
	result := make([]int, len(parts))
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			return nil, ports.Invalidf("invalid attachment part")
		}
		result[i] = n
	}
	return result, nil
}

func (s *Supervisor) SyncDraft(ctx context.Context, draftID int64) error {
	draft, attachments, err := s.repo.GetDraft(ctx, draftID)
	if err != nil {
		return err
	}
	if draft.Status != "draft" && draft.Status != "failed" && draft.Status != "unknown" {
		return nil
	}
	account, err := s.repo.GetAccount(ctx, draft.AccountID)
	if err != nil {
		return err
	}
	mailbox, err := s.repo.GetMailboxByRole(ctx, draft.AccountID, "drafts")
	if err != nil {
		return ports.Unavailablef("remote drafts mailbox is unavailable")
	}
	to, err := decodeDraftAddresses(draft.ToJSON)
	if err != nil {
		return err
	}
	cc, err := decodeDraftAddresses(draft.CCJSON)
	if err != nil {
		return err
	}
	bcc, err := decodeDraftAddresses(draft.BCCJSON)
	if err != nil {
		return err
	}
	outgoingAttachments := make([]mailparser.OutgoingAttachment, 0, len(attachments))
	var closers []interface{ Close() error }
	defer func() {
		for _, closer := range closers {
			_ = closer.Close()
		}
	}()
	for _, attachment := range attachments {
		blob, err := s.repo.GetBlob(ctx, attachment.BlobID)
		if err != nil {
			return err
		}
		reader, err := s.blobs.Open(ctx, blob)
		if err != nil {
			return err
		}
		closers = append(closers, reader)
		outgoingAttachments = append(outgoingAttachments, mailparser.OutgoingAttachment{Filename: attachment.Filename, ContentType: attachment.ContentType, Data: reader})
	}
	payload, err := mailparser.Compose(mailparser.Outgoing{
		MessageID: draft.RFCMessageID, From: mail.Address{Name: account.DisplayName, Address: account.Email},
		To: to, CC: cc, BCC: bcc, Subject: draft.Subject, BodyText: draft.BodyText, Attachments: outgoingAttachments,
	})
	if err != nil {
		return err
	}
	rt, err := s.runtime(account.ID)
	if err != nil {
		return err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return ports.Unavailablef("account is offline")
	}
	command := client.Append(mailbox.RemoteName, int64(len(payload)), &goimap.AppendOptions{Flags: []goimap.Flag{goimap.FlagDraft}, Time: time.Now()})
	if _, err := command.Write(payload); err != nil {
		_ = command.Close()
		return err
	}
	if err := command.Close(); err != nil {
		return err
	}
	appended, err := command.Wait()
	if err != nil {
		return err
	}
	if draft.RemoteUID != nil && draft.RemoteUIDValidity != nil && *draft.RemoteUIDValidity == appended.UIDValidity {
		if _, selectErr := client.Select(mailbox.RemoteName, nil).Wait(); selectErr == nil {
			old := goimap.UIDSetNum(goimap.UID(*draft.RemoteUID))
			if _, storeErr := client.Store(old, &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Collect(); storeErr == nil && client.Caps().Has(goimap.CapUIDPlus) {
				_, _ = client.UIDExpunge(old).Collect()
			}
		}
	}
	now := time.Now().UnixMilli()
	return s.repo.UpdateDraftRemote(ctx, draft.ID, mailbox.ID, appended.UIDValidity, uint32(appended.UID), now, "synced", nil)
}

func (s *Supervisor) DeleteRemoteDraft(ctx context.Context, draftID int64) error {
	draft, _, err := s.repo.GetDraft(ctx, draftID)
	if err != nil {
		return err
	}
	if draft.RemoteMailboxID == nil || draft.RemoteUID == nil {
		return nil
	}
	mailbox, err := s.repo.GetMailboxByRole(ctx, draft.AccountID, "drafts")
	if err != nil {
		return err
	}
	rt, err := s.runtime(draft.AccountID)
	if err != nil {
		return err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return ports.Unavailablef("account is offline")
	}
	selected, err := client.Select(mailbox.RemoteName, nil).Wait()
	if err != nil {
		return err
	}
	if draft.RemoteUIDValidity != nil && selected.UIDValidity != *draft.RemoteUIDValidity {
		return nil
	}
	uids := goimap.UIDSetNum(goimap.UID(*draft.RemoteUID))
	if _, err := client.Store(uids, &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Collect(); err != nil {
		return err
	}
	if client.Caps().Has(goimap.CapUIDPlus) {
		_, err = client.UIDExpunge(uids).Collect()
		return err
	}
	return nil
}

func (s *Supervisor) AppendSent(ctx context.Context, accountID int64, payload []byte) error {
	mailbox, err := s.repo.GetMailboxByRole(ctx, accountID, "sent")
	if err != nil {
		return ports.Unavailablef("remote sent mailbox is unavailable")
	}
	rt, err := s.runtime(accountID)
	if err != nil {
		return err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return ports.Unavailablef("account is offline")
	}
	command := client.Append(mailbox.RemoteName, int64(len(payload)), &goimap.AppendOptions{Flags: []goimap.Flag{goimap.FlagSeen}, Time: time.Now()})
	if _, err := command.Write(payload); err != nil {
		_ = command.Close()
		return err
	}
	if err := command.Close(); err != nil {
		return err
	}
	_, err = command.Wait()
	return err
}

func decodeDraftAddresses(raw string) ([]mail.Address, error) {
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, err
	}
	result := make([]mail.Address, 0, len(values))
	for _, value := range values {
		address, err := mail.ParseAddress(value)
		if err != nil {
			return nil, err
		}
		result = append(result, *address)
	}
	return result, nil
}
