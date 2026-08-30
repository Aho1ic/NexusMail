package imap

import (
	"log/slog"
	"math/rand/v2"
	"time"
)

// Tuning constants for the IMAP provider, with the reasoning for each value.
// They are collected here because the intervals interact: the probe cadence, the
// periodic sync, the reconcile floor and the stall windows have to stay in a
// sane relation to each other, which is hard to see when each sits next to its
// caller.

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

// requestSyncCooldown is the minimum interval between two RequestMailbox
// signals for the same mailbox. The 5s probe and the IDLE loop are the
// authoritative source of new-mail detection; this channel only exists to
// express "the user just opened this folder, prefer not to wait the next
// probe tick", and a passive reader must not be able to amplify into
// sustained IMAP traffic.
const requestSyncCooldown = 5 * time.Second

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
