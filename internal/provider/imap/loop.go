package imap

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// The two goroutines each account runs: commandLoop, which owns the command
// connection and does all work, and idleLoop, which only listens and signals.

func (s *Supervisor) commandLoop(ctx context.Context, rt *runtime) {
	backoff := time.Second
	for ctx.Err() == nil {
		_ = s.repo.UpdateAccountStatus(ctx, rt.account.ID, "connecting", nil)
		client, err := s.connect(ctx, rt.account, nil, s.commandStallOrDefault())
		if err != nil {
			// A credential the provider has definitively refused parks the account
			// in auth_error instead of being hammered on the connection ladder; an
			// IMAP rejection that only looks like one is retried a few times first,
			// because Gmail issues those for valid tokens under throttling. A
			// throttled connect likewise needs the full rateLimitBackoff:
			// reconnecting every second keeps the throttle engaged. A refusal that
			// could be either — QQ names its login-frequency limit as one of five
			// causes for the same message — is corroborated on that same long
			// window. All of it lives in classifyFailure, which idleLoop shares, so
			// the two loops cannot drift apart on this.
			result := s.classifyFailure(rt, err, backoff)
			s.setError(ctx, rt.account.ID, result, err)
			if !waitBackoff(ctx, result.delay) {
				return
			}
			// The ladder advances only for the failures it actually governs.
			if result.ladder {
				backoff = min(backoff*2, 5*time.Minute)
			}
			continue
		}
		// Authentication succeeded, so any auth rejections counted against this
		// account were transient.
		rt.authSucceeded()
		rt.client.Store(client)
		_ = s.repo.UpdateAccountStatus(ctx, rt.account.ID, "syncing", nil)
		// Refresh the catalog without holding the command lock so the LIST
		// round-trip and mailbox upserts do not block the new-mail path.
		if _, listErr := s.refreshMailboxCatalog(ctx, rt, client); listErr != nil {
			s.setError(ctx, rt.account.ID, verdict{status: "backoff", delay: backoff, store: true, ladder: true}, listErr)
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
			syncResult := s.classifyFailure(rt, syncErr, backoff)
			// A sync that keeps failing used to reconnect with no delay at all,
			// because the ladder was reset on connect and only the connect path
			// waited. That is a tight reconnect loop against the provider.
			s.setError(ctx, rt.account.ID, syncResult, syncErr)
			s.closeCommand(rt, client)
			if !waitBackoff(ctx, syncResult.delay) {
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
				// servicePending, not a copy of its rules: it resolves the zero
				// sentinel to the inbox and refuses a mailbox belonging to another
				// account. Anything else queued behind it is drained in the same lock
				// acquisition rather than one request per pass through this select.
				err := s.servicePending(ctx, rt, client, mailboxID)
				if err == nil {
					err = s.drainPending(ctx, rt, client)
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
			// Classified through the same path as the command loop so the auth
			// evidence both loops gather lands in one counter. This loop only needs
			// the delay: the command loop owns the account's status and error text.
			delay := s.classifyFailure(rt, cause, backoff).delay
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
		// This connection authenticated, which retires any auth rejections counted
		// against the account by either loop.
		rt.authSucceeded()
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
			if !s.pollWithoutIdle(ctx, rt, realtimePollInterval, periodicSyncInterval) {
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
//
// The two cadences are parameters rather than the package constants because no
// IMAP server this codebase talks to — nor the in-memory one the tests run —
// can be made to withhold IDLE: go-imap advertises it for both IMAP4rev1 and
// IMAP4rev2, so the only way to exercise this loop is to call it.
func (s *Supervisor) pollWithoutIdle(ctx context.Context, rt *runtime, poll, recheckAfter time.Duration) bool {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	recheck := time.NewTimer(recheckAfter)
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
