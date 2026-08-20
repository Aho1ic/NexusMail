//go:build sqlite_fts5

package send

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/platform/cryptobox"
	"nexusmail/internal/ports"
	smtpprovider "nexusmail/internal/provider/smtp"
	"nexusmail/internal/repository/sqlite"
	accountservice "nexusmail/internal/service/account"
	"nexusmail/internal/storage"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

// TestDeliverSent walks the success path end to end against a real SMTP
// conversation: the draft becomes sent, and a message appears in Sent.
func TestDeliverSent(t *testing.T) {
	harness := newHarness(t, backend{})
	draft := harness.queueDraft(t, "Sent path", "hello")

	harness.worker.deliver(context.Background(), draft.ID)

	stored := harness.draft(t, draft.ID)
	if stored.Status != "sent" {
		t.Fatalf("status = %q, want sent, last_error=%s", stored.Status, errText(stored))
	}
	if stored.SentAt == nil {
		t.Fatal("sent_at not recorded")
	}
	if len(harness.backend.received) != 1 {
		t.Fatalf("server received %d messages", len(harness.backend.received))
	}
	if !strings.Contains(harness.backend.received[0], "Subject:") {
		t.Fatalf("delivered payload has no header block: %q", harness.backend.received[0])
	}
	if harness.statuses() != "sent" {
		t.Fatalf("published statuses = %q", harness.statuses())
	}
	// The sent copy is what the user sees in the Sent folder; a delivery that does
	// not produce it looks like the mail vanished.
	page, err := harness.repo.ListMessages(context.Background(), ports.MessageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Direction != "outgoing" {
		t.Fatalf("sent copy missing: %#v", page.Items)
	}
}

// TestDeliverPermanentFailure covers a 5xx: no retry, because the server has
// already made a final decision and retrying only burns the account's reputation.
func TestDeliverPermanentFailure(t *testing.T) {
	harness := newHarness(t, backend{rcptErr: &gosmtp.SMTPError{Code: 550, Message: "no such user"}})
	draft := harness.queueDraft(t, "Permanent", "hello")

	harness.worker.deliver(context.Background(), draft.ID)

	stored := harness.draft(t, draft.ID)
	if stored.Status != "failed" {
		t.Fatalf("status = %q, want failed", stored.Status)
	}
	if stored.NextAttemptAt != nil {
		t.Fatalf("permanent failure scheduled a retry at %d", *stored.NextAttemptAt)
	}
	if stored.LastSMTPCode == nil || *stored.LastSMTPCode != 550 {
		t.Fatalf("smtp code not recorded: %v", stored.LastSMTPCode)
	}
}

// TestDeliverTemporaryFailureRetries covers a 4xx: retry_wait with a scheduled
// next attempt, which is what makes a greylisting provider eventually succeed.
func TestDeliverTemporaryFailureRetries(t *testing.T) {
	harness := newHarness(t, backend{rcptErr: &gosmtp.SMTPError{Code: 451, Message: "try again later"}})
	draft := harness.queueDraft(t, "Temporary", "hello")

	before := time.Now().UnixMilli()
	harness.worker.deliver(context.Background(), draft.ID)

	stored := harness.draft(t, draft.ID)
	if stored.Status != "retry_wait" {
		t.Fatalf("status = %q, want retry_wait: %s", stored.Status, errText(stored))
	}
	// The recorded code has to be the scripted 451. Asserting only on the status lets
	// this pass on any temporary failure, including one where the conversation never
	// reached RCPT at all.
	if stored.LastSMTPCode == nil || *stored.LastSMTPCode != 451 {
		t.Fatalf("smtp code = %v, want 451 (%s)", stored.LastSMTPCode, errText(stored))
	}
	if stored.NextAttemptAt == nil {
		t.Fatal("no retry scheduled for a temporary failure")
	}
	// First attempt uses the 5s rung of the ladder.
	if delay := *stored.NextAttemptAt - before; delay < 4_000 || delay > 6_500 {
		t.Fatalf("first retry delay = %dms, want about 5000", delay)
	}
	// A retry_wait draft becomes due on its own, without user action.
	due, err := harness.repo.ListDueDraftIDs(context.Background(), *stored.NextAttemptAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0] != draft.ID {
		t.Fatalf("draft not due at its scheduled time: %#v", due)
	}
}

// TestRetryLadderAndAttemptCap pins both halves of the backoff contract: the delays
// grow, and attempt 5 is the last one. Without the cap a permanently broken account
// retries forever.
func TestRetryLadderAndAttemptCap(t *testing.T) {
	harness := newHarness(t, backend{rcptErr: &gosmtp.SMTPError{Code: 451, Message: "try again later"}})
	draft := harness.queueDraft(t, "Ladder", "hello")
	ctx := context.Background()

	wantDelays := []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}
	for attempt, want := range wantDelays {
		before := time.Now().UnixMilli()
		harness.worker.deliver(ctx, draft.ID)
		stored := harness.draft(t, draft.ID)
		if stored.AttemptCount != attempt+1 {
			t.Fatalf("attempt_count = %d after %d deliveries", stored.AttemptCount, attempt+1)
		}
		if attempt == len(wantDelays)-1 {
			// The fifth attempt exhausts the budget, so this one is terminal.
			if stored.Status != "failed" {
				t.Fatalf("attempt 5 status = %q, want failed", stored.Status)
			}
			if stored.NextAttemptAt != nil {
				t.Fatal("attempt 5 scheduled a sixth")
			}
			break
		}
		if stored.Status != "retry_wait" || stored.NextAttemptAt == nil {
			t.Fatalf("attempt %d status = %q next=%v (%s)", attempt+1, stored.Status, stored.NextAttemptAt, errText(stored))
		}
		if stored.LastSMTPCode == nil || *stored.LastSMTPCode != 451 {
			t.Fatalf("attempt %d smtp code = %v, want 451 (%s)", attempt+1, stored.LastSMTPCode, errText(stored))
		}
		delay := time.Duration(*stored.NextAttemptAt-before) * time.Millisecond
		if delay < want-time.Second || delay > want+2*time.Second {
			t.Fatalf("attempt %d delay = %v, want about %v", attempt+1, delay, want)
		}
		// Move the draft back to a claimable state for the next round, the way the
		// scheduler does when the retry comes due.
		if err := harness.repo.SetDraftDelivery(ctx, draft.ID, "retry_wait", stored.AttemptCount, ptr(time.Now().UnixMilli()), nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDeliverUnknownIsTerminal is the most important case in the state machine. The
// connection dropped after DATA, so the message may well have been accepted;
// retrying would deliver it twice. The draft has to stop here and wait for a person.
func TestDeliverUnknownIsTerminal(t *testing.T) {
	harness := newHarness(t, backend{dropAfterData: true})
	draft := harness.queueDraft(t, "Unknown", "hello")
	ctx := context.Background()

	harness.worker.deliver(ctx, draft.ID)

	stored := harness.draft(t, draft.ID)
	if stored.Status != "unknown" {
		t.Fatalf("status = %q, want unknown, last_error=%s", stored.Status, errText(stored))
	}
	if stored.NextAttemptAt != nil {
		t.Fatalf("unknown scheduled an automatic retry at %d", *stored.NextAttemptAt)
	}
	// Nothing background may pick it up again: not the due-draft scan now, and not
	// after any amount of time has passed.
	due, err := harness.repo.ListDueDraftIDs(ctx, time.Now().Add(365*24*time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range due {
		if id == draft.ID {
			t.Fatal("unknown draft was picked up for automatic retry")
		}
	}
	// A second deliver call must also be refused, so a stray enqueue cannot resend.
	harness.worker.deliver(ctx, draft.ID)
	if again := harness.draft(t, draft.ID); again.AttemptCount != stored.AttemptCount {
		t.Fatalf("attempt_count moved from %d to %d without a user retry", stored.AttemptCount, again.AttemptCount)
	}
	// An explicit user retry is still allowed: only the automatic path is closed.
	if err := harness.worker.Queue(ctx, draft.ID); err != nil {
		t.Fatalf("explicit retry refused: %v", err)
	}
	if queued := harness.draft(t, draft.ID); queued.Status != "queued" {
		t.Fatalf("explicit retry left status %q", queued.Status)
	}
}

// TestQueueRejectsInFlightDraft guards against a double send from the API: a draft
// already being delivered cannot be queued again.
func TestQueueRejectsInFlightDraft(t *testing.T) {
	harness := newHarness(t, backend{})
	draft := harness.queueDraft(t, "In flight", "hello")
	ctx := context.Background()

	if _, _, err := harness.repo.ClaimSendableDraft(ctx, draft.ID); err != nil {
		t.Fatal(err)
	}
	if err := harness.worker.Queue(ctx, draft.ID); err == nil {
		t.Fatal("a sending draft was accepted for queueing")
	}
	if err := harness.repo.SetDraftDelivery(ctx, draft.ID, "sent", 1, nil, nil, nil, ptr(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	if err := harness.worker.Queue(ctx, draft.ID); err == nil {
		t.Fatal("a sent draft was accepted for queueing")
	}
}

// TestDeliverRejectsOversizeMessage checks the local guard runs before the network:
// the composed size is known, so an oversize send fails without opening a
// connection and without consuming a retry budget on the provider.
func TestDeliverRejectsOversizeMessage(t *testing.T) {
	harness := newHarness(t, backend{})
	harness.worker.maxBytes = 32
	draft := harness.queueDraft(t, "Oversize", strings.Repeat("body ", 200))

	harness.worker.deliver(context.Background(), draft.ID)

	stored := harness.draft(t, draft.ID)
	if stored.Status != "failed" {
		t.Fatalf("status = %q, want failed", stored.Status)
	}
	if len(harness.backend.received) != 0 {
		t.Fatal("oversize message was transmitted")
	}
	if stored.LastError == nil || !strings.Contains(*stored.LastError, "exceeds") {
		t.Fatalf("last_error = %v", stored.LastError)
	}
}

// TestDeliverRejectsMalformedRecipient covers a draft that reached the queue with an
// address the API would have rejected — stored by an older build, or edited through
// IMAP by another client. Composition has to fail it locally rather than open a
// connection and let the provider refuse it.
func TestDeliverRejectsMalformedRecipient(t *testing.T) {
	harness := newHarness(t, backend{})
	ctx := context.Background()
	draft := harness.queueDraftWith(t, "Bad recipient", "hello", `["not an address"]`)

	harness.worker.deliver(ctx, draft.ID)
	if stored := harness.draft(t, draft.ID); stored.Status != "failed" {
		t.Fatalf("status = %q, want failed", stored.Status)
	}
	if len(harness.backend.received) != 0 {
		t.Fatal("a message with an unparseable recipient was transmitted")
	}
}

type harness struct {
	repo    ports.Repository
	worker  *Worker
	account domain.Account
	backend *backend
	events  *recorder
}

func newHarness(t *testing.T, behaviour backend) *harness {
	t.Helper()
	root := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(root, "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	blobs, err := storage.New(filepath.Join(root, "blobs"), 1<<20, repo)
	if err != nil {
		t.Fatal(err)
	}
	box, err := cryptobox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}

	server := &behaviour
	host, port, roots := startSMTPServer(t, server)
	credential, err := json.Marshal(accountservice.Credential{Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal(credential)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	account := domain.Account{
		Email: "sender@example.com", DisplayName: "Sender", Provider: "qq", AuthType: "password", Username: "sender@example.com",
		IMAPHost: "127.0.0.1", IMAPPort: 993, IMAPTLSMode: "implicit",
		// Implicit TLS because the schema only permits implicit or starttls, and
		// because it is what the presets these tests borrow from actually use.
		SMTPHost: host, SMTPPort: port, SMTPTLSMode: "implicit",
		SecretCiphertext: sealed, Status: "connected", CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.CreateAccount(context.Background(), &account); err != nil {
		t.Fatal(err)
	}

	events := &recorder{}
	worker := New(repo, blobs, accountservice.New(repo, box), stubTokens{}, smtpprovider.NewWithRoots(5*time.Second, roots), events, 1<<20, nil)
	return &harness{repo: repo, worker: worker, account: account, backend: server, events: events}
}

func (h *harness) queueDraft(t *testing.T, subject, body string) domain.Draft {
	t.Helper()
	return h.queueDraftWith(t, subject, body, `["recipient@example.com"]`)
}

func (h *harness) queueDraftWith(t *testing.T, subject, body, toJSON string) domain.Draft {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	draft := domain.Draft{
		AccountID: h.account.ID, Revision: 1, RFCMessageID: "<" + subject + "@example.com>",
		ToJSON: toJSON, CCJSON: "[]", BCCJSON: "[]",
		Subject: subject, BodyText: body, Status: "draft", RemoteSyncState: "dirty",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := h.repo.CreateDraft(ctx, &draft); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.SetDraftDelivery(ctx, draft.ID, "queued", 0, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	h.events.reset()
	return draft
}

func (h *harness) draft(t *testing.T, id int64) domain.Draft {
	t.Helper()
	draft, _, err := h.repo.GetDraft(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return draft
}

func (h *harness) statuses() string {
	h.events.mu.Lock()
	defer h.events.mu.Unlock()
	return strings.Join(h.events.statuses, ",")
}

type recorder struct {
	mu       sync.Mutex
	statuses []string
}

func (r *recorder) Publish(event ports.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, ok := event.Data.(map[string]any)
	if !ok {
		return
	}
	if status, ok := data["status"].(string); ok {
		r.statuses = append(r.statuses, status)
	}
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = nil
}

type stubTokens struct{}

func (stubTokens) AccessToken(context.Context, domain.Account, string) (string, error) {
	return "", errors.New("oauth is not exercised by these tests")
}

// backend is a scriptable SMTP server. Driving the real protocol is what makes the
// unknown case reachable: it only exists because the connection can die between the
// last byte of DATA and the server's reply.
type backend struct {
	rcptErr       error
	dataErr       error
	dropAfterData bool
	received      []string
}

func (b *backend) NewSession(conn *gosmtp.Conn) (gosmtp.Session, error) {
	return &session{backend: b, conn: conn}, nil
}

type session struct {
	backend *backend
	conn    *gosmtp.Conn
}

func (s *session) Reset()                                 {}
func (s *session) Logout() error                          { return nil }
func (s *session) Mail(string, *gosmtp.MailOptions) error { return nil }
func (s *session) Rcpt(string, *gosmtp.RcptOptions) error { return s.backend.rcptErr }

// The client authenticates with LOGIN, which go-sasl has no server for, so the
// exchange is scripted here. Credentials are not what these tests check; reaching
// MAIL FROM is.
func (s *session) AuthMechanisms() []string         { return []string{"LOGIN"} }
func (s *session) Auth(string) (sasl.Server, error) { return &loginServer{}, nil }

type loginServer struct{ step int }

// The client sends the username as its initial response and then expects exactly
// "Password:" as the challenge, so the sequence is fixed rather than a stub.
func (l *loginServer) Next([]byte) ([]byte, bool, error) {
	l.step++
	if l.step == 1 {
		return []byte("Password:"), false, nil
	}
	return nil, true, nil
}

func (s *session) Data(reader io.Reader) error {
	body, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if s.backend.dropAfterData {
		// The body was accepted and then the connection went away before the reply, so
		// the client cannot know whether the message was queued.
		_ = s.conn.Conn().Close()
		return errors.New("connection lost")
	}
	if s.backend.dataErr != nil {
		return s.backend.dataErr
	}
	s.backend.received = append(s.backend.received, string(body))
	return nil
}

// startSMTPServer runs a real SMTP server on loopback behind implicit TLS and
// returns the certificate pool the client needs to trust it. A real server, rather
// than a stub client, is what makes the unknown-result case reachable: it only
// exists because the connection can die between the last byte of DATA and the reply.
func startSMTPServer(t *testing.T, handler gosmtp.Backend) (string, int, *x509.CertPool) {
	t.Helper()
	certificate, roots := selfSignedCert(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().(*net.TCPAddr)
	secure := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})

	server := gosmtp.NewServer(handler)
	server.Domain = "localhost"
	server.ReadTimeout = 5 * time.Second
	server.WriteTimeout = 5 * time.Second
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(secure) }()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
	})
	return address.IP.String(), address.Port, roots
}

func selfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
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

func errText(draft domain.Draft) string {
	if draft.LastError == nil {
		return "<nil>"
	}
	return *draft.LastError
}

func ptr[T any](value T) *T { return &value }
