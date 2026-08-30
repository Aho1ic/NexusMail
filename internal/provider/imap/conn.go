package imap

import (
	"context"
	"crypto/tls"
	"log/slog"
	"mime"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"nexusmail/internal/domain"
	providerauth "nexusmail/internal/provider/auth"
	"nexusmail/internal/version"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	messagecharset "github.com/emersion/go-message/charset"
	"github.com/emersion/go-sasl"
)

// Connection setup: dialling, TLS, authentication, and the read/write stall
// guard that turns a silently dead connection into an error.

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
