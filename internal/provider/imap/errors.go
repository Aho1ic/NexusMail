package imap

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"nexusmail/internal/ports"
	providerauth "nexusmail/internal/provider/auth"

	goimap "github.com/emersion/go-imap/v2"
)

// Error classification and the retry/backoff arithmetic shared by the loops.

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
		"password error",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// credentialsRejected reports whether the credential itself has been refused by
// something that knows, rather than inferred from an IMAP rejection that may be
// transient. Only the token path can say this: it sees the OAuth error code.
func credentialsRejected(err error) bool {
	return err != nil && errors.Is(err, providerauth.ErrCredentialRejected)
}

// isAmbiguousLoginRejection reports whether the provider refused the login
// without saying why. QQ answers a failed LOGIN with a single message that lists
// every cause it could be — "Login fail. Account is abnormal, service is not
// open, password is incorrect, login frequency limited, or system is busy" — so
// the text is evidence that the login was refused and nothing beyond that. Two
// of those causes need the user (the password is wrong, IMAP is not enabled) and
// two clear on their own (the frequency limit, the busy server), and the string
// is byte-identical either way.
//
// It is matched ahead of isAuthFailure and isRateLimited rather than folded into
// either, because it is honestly both and the handling it needs is neither: the
// corroboration an inferred auth failure gets before the user is told, on the
// long window a throttle gets so that retrying cannot be what keeps the account
// refused. Note that isRateLimited does not match this message ("system is busy"
// is not its "system busy" marker) and must not be widened until it does — that
// would route a wrong password onto a window that retries silently forever.
//
// The match is deliberately just "login fail" and not the full list. QQ appends a
// help URL and has reworded the causes before, and the two failure modes are not
// symmetric: matching too widely costs another provider's definitive rejection
// some reporting latency, while matching too narrowly drops this message back on
// the 1s ladder, which is the fault being fixed.
func isAmbiguousLoginRejection(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "login fail")
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

// setError records a failure against the account. The verdict decides whether the
// message reaches the user: a failure that heals on its own clears last_error
// instead, while the full text still goes to slog and the realtime event for
// operators and tools that need it.
func (s *Supervisor) setError(ctx context.Context, id int64, result verdict, err error) {
	var stored *string
	if result.store {
		text := err.Error()
		stored = &text
	}
	if updateErr := s.repo.UpdateAccountStatus(ctx, id, result.status, stored); updateErr != nil {
		slog.Error("mail account status update failed", "account_id", id, "status", result.status, "error", updateErr)
	}
	slog.Error("mail account sync failed", "account_id", id, "status", result.status, "error", err.Error())
	s.events.Publish(ports.Event{Type: "ACCOUNT_STATUS", Data: map[string]any{"account_id": id, "status": result.status, "error": err.Error()}})
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

// verdict is what a failure means for the account: the status to record, whether
// the message is worth showing the user, and how long to leave the provider alone.
type verdict struct {
	status string
	delay  time.Duration
	// store is false for a failure that heals on its own. The message still goes
	// to slog and the realtime event; it just does not become the red banner in
	// the sidebar, which stays until something overwrites last_error.
	store bool
	// ladder is true when delay came from the caller's 1s→5m progression, which is
	// the only case where advancing it means anything. The auth and throttle
	// windows are fixed, so doubling the ladder underneath one of them would leave
	// a long delay behind after the real fault cleared.
	ladder bool
}

// classifyFailure decides what a post-greeting failure means for the account.
//
// An IMAP AUTHENTICATIONFAILED is deliberately *not* trusted on its own. Gmail
// returns "[AUTHENTICATIONFAILED] Invalid credentials (Failure)" for a perfectly
// valid OAuth token when it is throttling authentication attempts — measured on
// this deployment as 11 rejections over 6 days on an account that was connected
// before and after each one, several of them right after a burst of reconnects.
// Treating the first one as final parked a healthy account for the full 15-minute
// auth window and told the user their credentials were invalid, which is both
// wrong and the one message they cannot dismiss.
//
// So an inferred auth failure has to be corroborated: it retries on a window long
// enough not to hammer the provider but far shorter than the auth park, and only
// after maxAuthFailures consecutive rejections — across both loops, since neither
// alone proves much — is the account parked and the user told. A rejection the
// token path attributes to the credential itself needs no corroboration: an
// invalid_grant is already the provider's final answer. A rejection that names its
// own possible causes and includes a throttle among them (QQ's "Login fail") takes
// the same corroboration on a longer window — see isAmbiguousLoginRejection.
// The evidence it weighs is per account — two accounts on the same provider fail
// independently — so the counter lives on the runtime while the window it retries
// on comes from the Supervisor, alongside the other tunable timings.
func (s *Supervisor) classifyFailure(rt *runtime, err error, ladder time.Duration) verdict {
	switch {
	case credentialsRejected(err):
		return verdict{status: "auth_error", delay: authBackoff, store: true}
	case isAmbiguousLoginRejection(err):
		// A refusal that is its own possible cause: it may be the credential, and
		// it may be the login-frequency limit that our own retries would keep
		// engaged. Corroborated like an auth failure so the user is only told once
		// it persists, but waited out on the throttle window, because at
		// authRetryBackoff both loops would sustain ~2 logins a minute against
		// exactly the limit the message names.
		return s.corroborateAuth(rt, rateLimitBackoff)
	case isAuthFailure(err):
		return s.corroborateAuth(rt, s.authRetryOrDefault())
	case isRateLimited(err):
		// A rate-limit error is self-healing: the loop waits rateLimitBackoff and
		// retries on its own. Storing the raw "imap: NO System busy!" string would
		// replay the same message to the user on every retry, which is exactly the
		// noise we are trying to avoid. The amber "backoff" dot already signals
		// "trying, just slowly".
		return verdict{status: "backoff", delay: rateLimitBackoff, store: false}
	}
	return verdict{status: "backoff", delay: ladder, store: true, ladder: true}
}

// corroborateAuth counts one rejection against the account and decides whether it
// is believed yet. Below the threshold the account backs off for retry with no
// stored message, so a rejection the provider issued transiently never reaches the
// user; at the threshold it is parked in auth_error with the text, because by then
// only the user can fix it. retry is how long an unbelieved rejection waits, which
// differs by how much the message itself could be a throttle.
func (s *Supervisor) corroborateAuth(rt *runtime, retry time.Duration) verdict {
	if rt.authFailures.Add(1) >= maxAuthFailures {
		return verdict{status: "auth_error", delay: authBackoff, store: true}
	}
	return verdict{status: "backoff", delay: retry, store: false}
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
