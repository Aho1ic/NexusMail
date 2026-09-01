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
// invalid_grant is already the provider's final answer.
// The evidence it weighs is per account — two accounts on the same provider fail
// independently — so the counter lives on the runtime while the window it retries
// on comes from the Supervisor, alongside the other tunable timings.
func (s *Supervisor) classifyFailure(rt *runtime, err error, ladder time.Duration) verdict {
	switch {
	case credentialsRejected(err):
		return verdict{status: "auth_error", delay: authBackoff, store: true}
	case isAuthFailure(err):
		if rt.authFailures.Add(1) >= maxAuthFailures {
			return verdict{status: "auth_error", delay: authBackoff, store: true}
		}
		// Not yet believed. Reported as backoff with no stored message so a
		// transient rejection is invisible to the user, exactly like a throttle.
		return verdict{status: "backoff", delay: s.authRetryOrDefault(), store: false}
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
