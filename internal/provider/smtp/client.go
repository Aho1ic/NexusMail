package smtp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"nexusmail/internal/domain"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

type Credential struct {
	Password    string
	AccessToken string
}

type DeliveryError struct {
	Stage     string
	Code      int
	Temporary bool
	Unknown   bool
	Err       error
}

func (e *DeliveryError) Error() string { return fmt.Sprintf("SMTP %s: %v", e.Stage, e.Err) }
func (e *DeliveryError) Unwrap() error { return e.Err }

type Client struct {
	timeout time.Duration
	roots   *x509.CertPool
}

func New(timeout time.Duration) *Client { return &Client{timeout: timeout} }

// NewWithRoots validates server certificates against roots instead of the system
// trust store. A self-hosted deployment relaying through a server with a private CA
// needs this, and it is what lets the delivery tests drive a real SMTP conversation
// against a loopback server.
func NewWithRoots(timeout time.Duration, roots *x509.CertPool) *Client {
	return &Client{timeout: timeout, roots: roots}
}

// dataStallWindow bounds how long a DATA transfer may make no progress. The
// deadline is refreshed on every chunk that lands, so a slow uplink stays alive
// as long as it keeps moving; only a genuine stall trips it. Sizing the deadline
// off messageSize instead would either strand large attachments or hand tiny ones
// an implausibly long grace period.
const dataStallWindow = 60 * time.Second

// commitTimeoutFactor stretches the deadline for the dot that ends DATA. The
// server does its content scanning there, and that regularly outlasts the
// per-command timeout on a message with attachments. A timeout here is the worst
// outcome available: the message may already be delivered, so the draft becomes a
// permanent "unknown" that is never retried.
const commitTimeoutFactor = 4

func (c *Client) Send(ctx context.Context, account domain.Account, credential Credential, envelopeFrom string, recipients []string, messageSize int64, message io.Reader) error {
	address := net.JoinHostPort(account.SMTPHost, fmt.Sprintf("%d", account.SMTPPort))
	dialer := &net.Dialer{Timeout: c.timeout}
	var conn net.Conn
	var err error
	tlsConfig := &tls.Config{ServerName: account.SMTPHost, MinVersion: tls.VersionTLS12, RootCAs: c.roots}
	if account.SMTPTLSMode == "implicit" {
		conn, err = tls.DialWithDialer(dialer, "tcp", address, tlsConfig)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return classify("connect", err, false)
	}
	defer conn.Close()
	// The deadline is per phase, not per session. One deadline covering the whole
	// conversation means the clock keeps running through the body transfer, so a
	// large attachment on a slow link times out mid-DATA and lands in the
	// irrecoverable "unknown" state.
	phase := func() { _ = conn.SetDeadline(time.Now().Add(c.timeout)) }
	phase()
	var client *gosmtp.Client
	if account.SMTPTLSMode == "starttls" {
		client, err = gosmtp.NewClientStartTLS(conn, tlsConfig)
		if err != nil {
			return classify("starttls", err, false)
		}
	} else {
		client = gosmtp.NewClient(conn)
	}
	defer client.Close()
	var auth sasl.Client
	if account.AuthType == "oauth2" {
		auth = &xoauth2Client{username: account.Username, token: credential.AccessToken}
	} else {
		auth = sasl.NewLoginClient(account.Username, credential.Password)
	}
	phase()
	if err := client.Auth(auth); err != nil {
		return classify("auth", err, false)
	}
	var mailOptions *gosmtp.MailOptions
	if maximum, ok := client.MaxMessageSize(); ok {
		if messageSize > int64(maximum) {
			return &DeliveryError{Stage: "size", Code: 552, Err: fmt.Errorf("message size %d exceeds server limit %d", messageSize, maximum)}
		}
		mailOptions = &gosmtp.MailOptions{Size: messageSize}
	}
	phase()
	if err := client.Mail(envelopeFrom, mailOptions); err != nil {
		return classify("mail-from", err, false)
	}
	for _, recipient := range recipients {
		phase()
		if err := client.Rcpt(recipient, nil); err != nil {
			return classify("rcpt-to", err, false)
		}
	}
	phase()
	dataWriter, err := client.Data()
	if err != nil {
		return classify("data", err, false)
	}
	if _, err := io.Copy(dataWriter, &progressReader{source: message, conn: conn, window: dataStallWindow}); err != nil {
		_ = dataWriter.Close()
		return &DeliveryError{Stage: "data-write", Temporary: true, Unknown: true, Err: err}
	}
	_ = conn.SetDeadline(time.Now().Add(c.timeout * commitTimeoutFactor))
	if err := dataWriter.Close(); err != nil {
		return classify("data-commit", err, true)
	}
	phase()
	_ = client.Quit()
	return nil
}

// progressReader pushes the connection deadline forward as the body streams. It
// wraps the source rather than the writer so the refresh happens before the bytes
// are handed to the socket, which is the ordering that matters: the deadline must
// already cover the write that is about to block.
type progressReader struct {
	source io.Reader
	conn   net.Conn
	window time.Duration
}

func (r *progressReader) Read(p []byte) (int, error) {
	_ = r.conn.SetDeadline(time.Now().Add(r.window))
	return r.source.Read(p)
}

func classify(stage string, err error, ambiguous bool) error {
	result := &DeliveryError{Stage: stage, Err: err, Unknown: ambiguous}
	var smtpErr *gosmtp.SMTPError
	if errors.As(err, &smtpErr) {
		result.Code = smtpErr.Code
		result.Temporary = smtpErr.Code >= 400 && smtpErr.Code < 500
		result.Unknown = false
	} else {
		result.Temporary = !ambiguous
	}
	return result
}

type xoauth2Client struct {
	username   string
	token      string
	challenged bool
}

func (c *xoauth2Client) Start() (string, []byte, error) {
	if c.username == "" || c.token == "" {
		return "", nil, errors.New("XOAUTH2 credentials are empty")
	}
	response := fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", c.username, c.token)
	return "XOAUTH2", []byte(response), nil
}

func (c *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	if c.challenged {
		return nil, errors.New("unexpected repeated XOAUTH2 challenge")
	}
	c.challenged = true
	if len(challenge) > 0 {
		if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(challenge))); err == nil {
			challenge = decoded
		}
	}
	return []byte{}, nil
}
