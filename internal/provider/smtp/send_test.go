package smtp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"nexusmail/internal/domain"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

// The tests below drive the real SMTP protocol against a loopback server. A stub
// client would not reach the cases that only exist because a conversation has
// phases: a rejection at RCPT is retryable per recipient, a failure at the dot
// that ends DATA may already have been delivered.

// scriptedBackend fails at whichever phase the test names.
type scriptedBackend struct {
	mu sync.Mutex

	authErr       error
	mailErr       error
	rcptErr       error
	dataErr       error
	dropAfterData bool

	mechanisms []string
	received   []string
	recipients []string
	mailFrom   string
	mailSize   int64

	// xoauth2Challenge, when set, is what the XOAUTH2 server sends back before
	// refusing the token. Real providers answer a rejected bearer this way: a
	// base64 JSON blob describing the reason, then a failure once the client has
	// acknowledged it with an empty line.
	xoauth2Challenge string
}

func (b *scriptedBackend) NewSession(conn *gosmtp.Conn) (gosmtp.Session, error) {
	return &scriptedSession{backend: b, conn: conn}, nil
}

func (b *scriptedBackend) snapshot() ([]string, []string, string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.received...), append([]string(nil), b.recipients...), b.mailFrom, b.mailSize
}

type scriptedSession struct {
	backend *scriptedBackend
	conn    *gosmtp.Conn
}

func (s *scriptedSession) Reset()        {}
func (s *scriptedSession) Logout() error { return nil }

func (s *scriptedSession) Mail(from string, options *gosmtp.MailOptions) error {
	s.backend.mu.Lock()
	s.backend.mailFrom = from
	if options != nil {
		s.backend.mailSize = options.Size
	}
	err := s.backend.mailErr
	s.backend.mu.Unlock()
	return err
}

func (s *scriptedSession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	if s.backend.rcptErr != nil {
		return s.backend.rcptErr
	}
	s.backend.recipients = append(s.backend.recipients, to)
	return nil
}

func (s *scriptedSession) AuthMechanisms() []string {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	if s.backend.mechanisms != nil {
		return s.backend.mechanisms
	}
	return []string{"LOGIN", "XOAUTH2"}
}

func (s *scriptedSession) Auth(mechanism string) (sasl.Server, error) {
	s.backend.mu.Lock()
	err := s.backend.authErr
	s.backend.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if mechanism == "XOAUTH2" {
		s.backend.mu.Lock()
		challenge := s.backend.xoauth2Challenge
		s.backend.mu.Unlock()
		return &xoauth2Server{challenge: challenge}, nil
	}
	return &scriptedLoginServer{}, nil
}

func (s *scriptedSession) Data(reader io.Reader) error {
	body, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	s.backend.mu.Lock()
	drop, dataErr := s.backend.dropAfterData, s.backend.dataErr
	if drop == false && dataErr == nil {
		s.backend.received = append(s.backend.received, string(body))
	}
	s.backend.mu.Unlock()
	if drop {
		// The body was accepted and then the connection went away before the reply,
		// so the client cannot know whether the message was queued.
		_ = s.conn.Conn().Close()
		return errors.New("connection lost")
	}
	return dataErr
}

// The client sends the username as its initial response and then expects exactly
// "Password:" as the challenge.
type scriptedLoginServer struct{ step int }

func (l *scriptedLoginServer) Next([]byte) ([]byte, bool, error) {
	l.step++
	if l.step == 1 {
		return []byte("Password:"), false, nil
	}
	return nil, true, nil
}

// xoauth2Server accepts the single base64 blob the XOAUTH2 client sends. It records
// it so a test can assert the exact wire format, which is what a provider rejects
// when it is wrong.
type xoauth2Server struct {
	initial   string
	challenge string
	steps     int
}

func (x *xoauth2Server) Next(response []byte) ([]byte, bool, error) {
	x.steps++
	if x.steps == 1 {
		x.initial = string(response)
		if x.challenge != "" {
			// Not done: the client owes an empty line before the refusal.
			return []byte(x.challenge), false, nil
		}
		return nil, true, nil
	}
	return nil, false, errors.New("535 5.7.8 authentication failed")
}

func startServer(t *testing.T, backend gosmtp.Backend, mode string) (domain.Account, *x509.CertPool) {
	t.Helper()
	certificate, roots := testCert(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().(*net.TCPAddr)
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}

	server := gosmtp.NewServer(backend)
	server.Domain = "localhost"
	server.ReadTimeout = 5 * time.Second
	server.WriteTimeout = 5 * time.Second
	server.MaxMessageBytes = 1 << 20

	served := listener
	switch mode {
	case "implicit":
		served = tls.NewListener(listener, tlsConfig)
	case "starttls":
		server.TLSConfig = tlsConfig
	default:
		// A cleartext server has to opt into cleartext auth, exactly as a real one
		// would; the client sends LOGIN either way.
		server.AllowInsecureAuth = true
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(served) }()
	t.Cleanup(func() {
		// The listener is closed here as well as through the server, because
		// Server.Close only closes the listeners Serve has already registered with it
		// and Serve registers as its first act. A Close that lands before that
		// goroutine is scheduled therefore closes nothing, leaving Serve blocked in
		// Accept on a listener nothing will ever close. Accept returns on this.
		_ = server.Close()
		_ = served.Close()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			// Bounded so a server that will not stop is reported here, against the
			// test that owns it. An unbounded wait instead stalls the whole package
			// until the go test timeout, whose failure names no test at all.
			t.Error("SMTP server did not stop")
		}
	})
	return domain.Account{
		Email: "sender@example.com", Username: "sender@example.com", AuthType: "password",
		SMTPHost: address.IP.String(), SMTPPort: address.Port, SMTPTLSMode: mode,
	}, roots
}

func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}, pool
}

const testMessage = "From: sender@example.com\r\nTo: to@example.com\r\nSubject: Hello\r\n\r\nBody.\r\n"

func send(t *testing.T, account domain.Account, roots *x509.CertPool, recipients []string) error {
	t.Helper()
	client := NewWithRoots(5*time.Second, roots)
	return client.Send(context.Background(), account, Credential{Password: "secret"},
		account.Email, recipients, int64(len(testMessage)), strings.NewReader(testMessage))
}

func deliveryError(t *testing.T, err error) *DeliveryError {
	t.Helper()
	var delivery *DeliveryError
	if !errors.As(err, &delivery) {
		t.Fatalf("error is %T (%v), want *DeliveryError", err, err)
	}
	return delivery
}

// Each TLS mode has to complete a real handshake and a real conversation. These
// are the three the provider presets use.
func TestSendOverEveryTLSMode(t *testing.T) {
	for _, mode := range []string{"implicit", "starttls", "none"} {
		t.Run(mode, func(t *testing.T) {
			backend := &scriptedBackend{}
			account, roots := startServer(t, backend, mode)
			if err := send(t, account, roots, []string{"to@example.com"}); err != nil {
				t.Fatalf("send over %s: %v", mode, err)
			}
			received, recipients, from, _ := backend.snapshot()
			if len(received) != 1 || !strings.Contains(received[0], "Subject: Hello") {
				t.Fatalf("server received %d messages: %q", len(received), received)
			}
			if from != "sender@example.com" {
				t.Fatalf("MAIL FROM = %q", from)
			}
			if len(recipients) != 1 || recipients[0] != "to@example.com" {
				t.Fatalf("RCPT TO = %v", recipients)
			}
		})
	}
}

// Every recipient gets its own RCPT, and all of them have to be sent before DATA.
func TestSendAddressesEveryRecipient(t *testing.T) {
	backend := &scriptedBackend{}
	account, roots := startServer(t, backend, "implicit")
	want := []string{"one@example.com", "two@example.com", "three@example.com"}
	if err := send(t, account, roots, want); err != nil {
		t.Fatal(err)
	}
	_, recipients, _, _ := backend.snapshot()
	if len(recipients) != len(want) {
		t.Fatalf("RCPT TO = %v, want %v", recipients, want)
	}
	for i, address := range want {
		if recipients[i] != address {
			t.Fatalf("recipient %d = %q, want %q", i, recipients[i], address)
		}
	}
}

// The SIZE extension is advertised by the server, so the client must declare the
// message size on MAIL FROM. A server that reserves space by SIZE would otherwise
// accept the transaction and then reject the body.
func TestSendDeclaresTheMessageSize(t *testing.T) {
	backend := &scriptedBackend{}
	account, roots := startServer(t, backend, "implicit")
	if err := send(t, account, roots, []string{"to@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, size := backend.snapshot(); size != int64(len(testMessage)) {
		t.Fatalf("declared SIZE = %d, want %d", size, len(testMessage))
	}
}

// A message larger than the server's advertised limit is rejected locally, before
// the body is uploaded. Sending it anyway would waste the whole transfer to learn
// what the greeting already said, and 552 is permanent, so a retry cannot help.
func TestSendRejectsAnOversizedMessageBeforeUploading(t *testing.T) {
	backend := &scriptedBackend{}
	account, roots := startServer(t, backend, "implicit")
	client := NewWithRoots(5*time.Second, roots)
	err := client.Send(context.Background(), account, Credential{Password: "secret"},
		account.Email, []string{"to@example.com"}, 1<<30, strings.NewReader(testMessage))
	delivery := deliveryError(t, err)
	if delivery.Stage != "size" || delivery.Code != 552 || delivery.Temporary {
		t.Fatalf("delivery = %#v", delivery)
	}
	if received, _, _, _ := backend.snapshot(); len(received) != 0 {
		t.Fatal("the oversized body was uploaded anyway")
	}
}

// A 4xx is temporary and retryable; a 5xx is not. This is the distinction the send
// worker's backoff ladder is built on, so it is checked per phase.
func TestSendClassifiesFailuresByPhase(t *testing.T) {
	cases := []struct {
		name      string
		script    func(*scriptedBackend)
		stage     string
		code      int
		temporary bool
	}{
		{"auth rejected", func(b *scriptedBackend) {
			b.authErr = &gosmtp.SMTPError{Code: 535, EnhancedCode: gosmtp.EnhancedCode{5, 7, 8}, Message: "bad credentials"}
		}, "auth", 535, false},
		{"mailbox unavailable", func(b *scriptedBackend) {
			b.mailErr = &gosmtp.SMTPError{Code: 450, EnhancedCode: gosmtp.EnhancedCode{4, 2, 1}, Message: "mailbox busy"}
		}, "mail-from", 450, true},
		{"recipient rejected", func(b *scriptedBackend) {
			b.rcptErr = &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 1, 1}, Message: "no such user"}
		}, "rcpt-to", 550, false},
		{"recipient throttled", func(b *scriptedBackend) {
			b.rcptErr = &gosmtp.SMTPError{Code: 452, EnhancedCode: gosmtp.EnhancedCode{4, 5, 3}, Message: "too many recipients"}
		}, "rcpt-to", 452, true},
		{"content rejected", func(b *scriptedBackend) {
			b.dataErr = &gosmtp.SMTPError{Code: 554, EnhancedCode: gosmtp.EnhancedCode{5, 6, 0}, Message: "spam"}
		}, "data-commit", 554, false},
		{"content deferred", func(b *scriptedBackend) {
			b.dataErr = &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 3, 0}, Message: "try later"}
		}, "data-commit", 451, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := &scriptedBackend{}
			tc.script(backend)
			account, roots := startServer(t, backend, "implicit")
			delivery := deliveryError(t, send(t, account, roots, []string{"to@example.com"}))
			if delivery.Stage != tc.stage {
				t.Fatalf("stage = %q, want %q (%v)", delivery.Stage, tc.stage, delivery)
			}
			if delivery.Code != tc.code || delivery.Temporary != tc.temporary {
				t.Fatalf("delivery = %#v", delivery)
			}
			// A coded reply is never ambiguous: the server said what it did.
			if delivery.Unknown {
				t.Fatalf("a %d reply was classified as unknown", tc.code)
			}
		})
	}
}

// The one case that must never be retried: the body was accepted and the
// connection died before the reply, so the message may already be delivered.
// Retrying would send it twice.
func TestSendReportsAnUnknownResultWhenTheConnectionDiesAfterData(t *testing.T) {
	backend := &scriptedBackend{dropAfterData: true}
	account, roots := startServer(t, backend, "implicit")
	delivery := deliveryError(t, send(t, account, roots, []string{"to@example.com"}))
	if !delivery.Unknown {
		t.Fatalf("delivery = %#v, want Unknown", delivery)
	}
	if delivery.Code != 0 {
		t.Fatalf("an uncoded failure reported code %d", delivery.Code)
	}
}

// A server that is not there at all is a connect failure, and connect failures are
// temporary: the account is fine, the network or the server is not.
func TestSendClassifiesAConnectFailure(t *testing.T) {
	account := domain.Account{
		Username: "sender@example.com", AuthType: "password",
		SMTPHost: "127.0.0.1", SMTPPort: 1, SMTPTLSMode: "implicit",
	}
	delivery := deliveryError(t, send(t, account, nil, []string{"to@example.com"}))
	if delivery.Stage != "connect" || !delivery.Temporary || delivery.Unknown {
		t.Fatalf("delivery = %#v", delivery)
	}
}

// An untrusted certificate must fail the handshake rather than fall back to
// cleartext. Passing no root pool means the system store, which does not contain
// the test CA.
func TestSendRefusesAnUntrustedCertificate(t *testing.T) {
	backend := &scriptedBackend{}
	account, _ := startServer(t, backend, "implicit")
	client := New(2 * time.Second)
	err := client.Send(context.Background(), account, Credential{Password: "secret"},
		account.Email, []string{"to@example.com"}, int64(len(testMessage)), strings.NewReader(testMessage))
	if err == nil {
		t.Fatal("an untrusted certificate was accepted")
	}
	if received, _, _, _ := backend.snapshot(); len(received) != 0 {
		t.Fatal("the message was delivered over an untrusted connection")
	}
}

// STARTTLS against a server that does not offer it must fail rather than continue
// in cleartext with the credential.
func TestSendFailsWhenStartTLSIsUnavailable(t *testing.T) {
	backend := &scriptedBackend{}
	account, roots := startServer(t, backend, "none")
	account.SMTPTLSMode = "starttls"
	delivery := deliveryError(t, send(t, account, roots, []string{"to@example.com"}))
	if delivery.Stage != "starttls" {
		t.Fatalf("stage = %q, want starttls (%v)", delivery.Stage, delivery)
	}
}

// OAuth accounts authenticate with XOAUTH2, and the blob has to be exactly the
// format the providers accept: user=…\x01auth=Bearer …\x01\x01.
func TestSendUsesXOAUTH2ForOAuthAccounts(t *testing.T) {
	backend := &scriptedBackend{mechanisms: []string{"XOAUTH2"}}
	account, roots := startServer(t, backend, "implicit")
	account.AuthType = "oauth2"
	client := NewWithRoots(5*time.Second, roots)
	err := client.Send(context.Background(), account, Credential{AccessToken: "ya29.token"},
		account.Email, []string{"to@example.com"}, int64(len(testMessage)), strings.NewReader(testMessage))
	if err != nil {
		t.Fatalf("XOAUTH2 send: %v", err)
	}
	if received, _, _, _ := backend.snapshot(); len(received) != 1 {
		t.Fatalf("received %d messages", len(received))
	}
}

// An OAuth account with no token must fail before anything is sent: an empty
// bearer would be rejected by the provider as a credential error and could burn
// the account's status.
func TestSendRejectsAnEmptyOAuthToken(t *testing.T) {
	backend := &scriptedBackend{mechanisms: []string{"XOAUTH2"}}
	account, roots := startServer(t, backend, "implicit")
	account.AuthType = "oauth2"
	client := NewWithRoots(5*time.Second, roots)
	err := client.Send(context.Background(), account, Credential{},
		account.Email, []string{"to@example.com"}, int64(len(testMessage)), strings.NewReader(testMessage))
	delivery := deliveryError(t, err)
	if delivery.Stage != "auth" {
		t.Fatalf("stage = %q, want auth (%v)", delivery.Stage, delivery)
	}
	if received, _, _, _ := backend.snapshot(); len(received) != 0 {
		t.Fatal("a message was sent with an empty bearer token")
	}
}

// A rejected bearer token is the most common OAuth failure in practice, and the
// provider explains it in a challenge rather than in the SMTP status. Without that
// text the account's last_error reads "authentication failed" for an expired
// token, a revoked grant and a mailbox with SMTP disabled alike — three problems
// with three different fixes.
func TestSendReportsTheProvidersOAuthFailureReason(t *testing.T) {
	reason := `{"status":"400","schemes":"Bearer","scope":"https://mail.google.com/"}`
	backend := &scriptedBackend{
		mechanisms:       []string{"XOAUTH2"},
		xoauth2Challenge: base64.StdEncoding.EncodeToString([]byte(reason)),
	}
	account, roots := startServer(t, backend, "implicit")
	account.AuthType = "oauth2"
	client := NewWithRoots(5*time.Second, roots)

	err := client.Send(context.Background(), account, Credential{AccessToken: "an-expired-token"},
		account.Email, []string{"to@example.com"}, int64(len(testMessage)), strings.NewReader(testMessage))
	delivery := deliveryError(t, err)
	if delivery.Stage != "auth" {
		t.Fatalf("stage = %q, want auth (%v)", delivery.Stage, delivery)
	}
	if !strings.Contains(delivery.Error(), `"status":"400"`) {
		t.Errorf("error is %q, want it to carry the provider's decoded reason", delivery.Error())
	}
	if received, _, _, _ := backend.snapshot(); len(received) != 0 {
		t.Fatal("a message was sent after the token was refused")
	}
}

// Not every provider base64s the blob. An undecodable challenge must still reach
// the error rather than being dropped for failing to decode.
func TestSendReportsAPlainOAuthFailureReason(t *testing.T) {
	backend := &scriptedBackend{mechanisms: []string{"XOAUTH2"}, xoauth2Challenge: "token expired at 2026-08-30"}
	account, roots := startServer(t, backend, "implicit")
	account.AuthType = "oauth2"
	client := NewWithRoots(5*time.Second, roots)

	err := client.Send(context.Background(), account, Credential{AccessToken: "an-expired-token"},
		account.Email, []string{"to@example.com"}, int64(len(testMessage)), strings.NewReader(testMessage))
	delivery := deliveryError(t, err)
	if !strings.Contains(delivery.Error(), "token expired at 2026-08-30") {
		t.Errorf("error is %q, want the raw challenge text", delivery.Error())
	}
}

// The challenge comes from the network, so it cannot be allowed to grow the error
// without bound — an error that large ends up in the account row and the log.
func TestSendTruncatesAnOversizedOAuthFailureReason(t *testing.T) {
	backend := &scriptedBackend{mechanisms: []string{"XOAUTH2"}, xoauth2Challenge: strings.Repeat("A", 4096)}
	account, roots := startServer(t, backend, "implicit")
	account.AuthType = "oauth2"
	client := NewWithRoots(5*time.Second, roots)

	err := client.Send(context.Background(), account, Credential{AccessToken: "a-token"},
		account.Email, []string{"to@example.com"}, int64(len(testMessage)), strings.NewReader(testMessage))
	delivery := deliveryError(t, err)
	if runs := strings.Count(delivery.Error(), "A"); runs > maxXOAUTH2Failure {
		t.Errorf("the error carried %d challenge bytes, want at most %d", runs, maxXOAUTH2Failure)
	}
}

// A second challenge is a protocol violation: the exchange is one round trip, and
// a server that keeps challenging would otherwise loop.
func TestXOAUTH2RefusesARepeatedChallenge(t *testing.T) {
	client := &xoauth2Client{username: "user@example.com", token: "a-token"}
	if _, err := client.Next([]byte("first")); err != nil {
		t.Fatalf("first challenge: %v", err)
	}
	if _, err := client.Next([]byte("second")); err == nil {
		t.Error("a repeated challenge was accepted")
	}
}

// The initial response is a fixed wire format. Empty credentials must fail before
// it is built rather than sending "user=\x01auth=Bearer \x01\x01".
func TestXOAUTH2RefusesEmptyCredentials(t *testing.T) {
	for _, client := range []*xoauth2Client{
		{username: "", token: "a-token"},
		{username: "user@example.com", token: ""},
	} {
		if _, _, err := client.Start(); err == nil {
			t.Errorf("Start accepted username=%q token=%q", client.username, client.token)
		}
	}
	client := &xoauth2Client{username: "user@example.com", token: "a-token"}
	mechanism, initial, err := client.Start()
	if err != nil {
		t.Fatal(err)
	}
	if mechanism != "XOAUTH2" {
		t.Errorf("mechanism is %q, want XOAUTH2", mechanism)
	}
	if want := "user=user@example.com\x01auth=Bearer a-token\x01\x01"; string(initial) != want {
		t.Errorf("initial response is %q, want %q", initial, want)
	}
}

// The body is streamed, not buffered, so a reader that fails mid-transfer has to be
// reported as unknown: bytes are already on the wire and the server may still
// accept what it has.
func TestSendReportsAFailedBodyReadAsUnknown(t *testing.T) {
	backend := &scriptedBackend{}
	account, roots := startServer(t, backend, "implicit")
	client := NewWithRoots(5*time.Second, roots)
	body := io.MultiReader(strings.NewReader("From: a@b.com\r\n\r\npartial"), &failingReader{err: errors.New("blob read failed")})
	err := client.Send(context.Background(), account, Credential{Password: "secret"},
		account.Email, []string{"to@example.com"}, 1024, body)
	delivery := deliveryError(t, err)
	if delivery.Stage != "data-write" || !delivery.Unknown || !delivery.Temporary {
		t.Fatalf("delivery = %#v", delivery)
	}
}

// The error text names the phase, which is what a user sees on a failed draft and
// what makes a provider complaint diagnosable. It must not carry the credential.
func TestDeliveryErrorTextNamesThePhaseAndHidesTheCredential(t *testing.T) {
	backend := &scriptedBackend{authErr: &gosmtp.SMTPError{Code: 535, Message: "authentication failed"}}
	account, roots := startServer(t, backend, "implicit")
	err := send(t, account, roots, []string{"to@example.com"})
	text := err.Error()
	if !strings.HasPrefix(text, "SMTP auth:") {
		t.Fatalf("error text = %q", text)
	}
	if strings.Contains(text, "secret") {
		t.Fatalf("the credential leaked into %q", text)
	}
}
