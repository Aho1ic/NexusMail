//go:build sqlite_fts5

package imap

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nexusmail/internal/domain"
)

// countingCloseConn reports how many times the socket underneath a connection was
// closed, which is the only way to tell a released connection from a leaked one.
type countingCloseConn struct {
	net.Conn
	closes *atomic.Int32
}

func (c countingCloseConn) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

// stubTokens stands in for the OAuth manager. err is what AccessToken reports, so a
// test can model a provider that is throttling rather than one that has revoked
// consent — the two are indistinguishable at this layer.
type stubTokens struct {
	token string
	err   error
	calls atomic.Int32
}

func (s *stubTokens) AccessToken(context.Context, domain.Account, string) (string, error) {
	s.calls.Add(1)
	if s.err != nil {
		return "", s.err
	}
	return s.token, nil
}

// oauthHarness rebuilds the harness supervisor with a token provider and an OAuth
// account, which newHarness cannot do: it stores a password account and passes nil
// for tokens, so every existing test takes the password branch of connect.
func oauthHarness(t *testing.T, tokens TokenProvider) (*harness, domain.Account, *atomic.Int32) {
	t.Helper()
	h := newHarness(t)
	h.supervisor.tokens = tokens

	account, err := h.accounts.AddOAuth(context.Background(), "gmail", "mail@example.com", "OAuth", "refresh-token-value")
	if err != nil {
		t.Fatal(err)
	}

	var closes atomic.Int32
	inner := h.supervisor.dial
	h.supervisor.dial = func(ctx context.Context, acct domain.Account) (net.Conn, error) {
		conn, dialErr := inner(ctx, acct)
		if dialErr != nil {
			return nil, dialErr
		}
		return countingCloseConn{Conn: conn, closes: &closes}, nil
	}
	return h, account, &closes
}

// A token fetch that fails has to close the connection it already opened. This is not
// a rare path: Gmail answers AUTHENTICATIONFAILED for a valid token while it is
// throttling, so a healthy account walks through here on every retry of the command
// loop's backoff ladder. A connection left open each time would accumulate sockets
// against the provider's per-account limit for as long as the throttle lasts, which
// is exactly when the account can least afford it.
func TestConnectClosesTheSocketWhenTheTokenFetchFails(t *testing.T) {
	tokens := &stubTokens{err: errors.New("token endpoint is throttling")}
	h, account, closes := oauthHarness(t, tokens)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := h.supervisor.connect(ctx, account, nil, commandStallWindow)
	if err == nil {
		if client != nil {
			_ = client.Close()
		}
		t.Fatal("connect succeeded with a token provider that refused to issue a token")
	}
	if client != nil {
		t.Error("connect returned a client alongside its error, so a caller could use a connection that failed to authenticate")
	}
	if tokens.calls.Load() != 1 {
		t.Errorf("AccessToken was called %d times, want 1", tokens.calls.Load())
	}
	// At least once, not exactly once: the client closes the socket on the way out
	// and its reader can close it again on the resulting EOF. What matters is that
	// the count is not zero.
	if got := closes.Load(); got == 0 {
		t.Error("the socket was never closed: a throttled account leaks one connection per retry, against the provider's per-account limit and at the moment it can least afford it")
	}
}

// An OAuth account must authenticate over XOAUTH2 and must not fall through to the
// password branch, because it has no password: the credential row holds a refresh
// token, so a fallback would send an empty secret and the provider would answer with
// a plain authentication failure. That is the worst possible shape for this bug —
// classifyFailure counts it as auth evidence, and after maxAuthFailures the account
// is parked in auth_error, which the UI presents as revoked consent. Per
// gmail-transient-auth-rejection there is no way to re-authorize from the UI.
//
// The in-memory server has no XOAUTH2 mechanism, so a successful OAuth handshake
// cannot be driven here. Its refusal is still the evidence the test needs: rejecting
// the mechanism proves the mechanism was offered, and a token fetched exactly once
// proves the OAuth branch ran instead of the password one.
func TestConnectOffersXOAUTH2AndDoesNotFallBackToAPassword(t *testing.T) {
	tokens := &stubTokens{token: "access-token-value"}
	h, account, _ := oauthHarness(t, tokens)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := h.supervisor.connect(ctx, account, nil, commandStallWindow)
	if client != nil {
		_ = client.Close()
	}
	if err == nil {
		t.Fatal("the in-memory server accepted XOAUTH2, so its refusal can no longer stand in for the mechanism being offered")
	}
	if !strings.Contains(err.Error(), "SASL") && !strings.Contains(err.Error(), "mechanism") {
		t.Errorf("error = %q, which does not name the mechanism: the connection may have fallen back to a password login", err)
	}
	if calls := tokens.calls.Load(); calls != 1 {
		t.Errorf("AccessToken was called %d times, want 1: an OAuth account must take the token path exactly once per connection", calls)
	}
}
