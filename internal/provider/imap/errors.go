package imap

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"nexusmail/internal/ports"

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
