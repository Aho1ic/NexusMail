//go:build sqlite_fts5

package imap

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nexusmail/internal/config"
	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
	smtpprovider "nexusmail/internal/provider/smtp"
	"nexusmail/internal/realtime"
	draftservice "nexusmail/internal/service/draft"
	messageservice "nexusmail/internal/service/message"
	sendservice "nexusmail/internal/service/send"
	sessionservice "nexusmail/internal/service/session"
	httptransport "nexusmail/internal/transport/http"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

// The tests in this file assemble the whole process the way cmd/server does and
// drive it through real HTTP requests against a real IMAP server and a real SMTP
// server. Everything else in the suite tests one layer with the others stubbed;
// this is the only place where an assertion covers the wiring itself — that a
// message the provider delivered is reachable over HTTP, that a body the client
// asks for is fetched from IMAP on demand, and that a draft posted to the API
// leaves via SMTP and comes back as a row plus a remote Sent copy.

const fullstackAPIKey = "fullstack-api-key-that-is-long-enough-32"

// mailbox returns a recorder for what the SMTP server actually received, so a
// send assertion can look at the wire bytes rather than at local state only.
type recordingSMTP struct {
	mu         sync.Mutex
	messages   []string
	recipients []string
	from       string
	authUser   string
	rejectWith error
}

func (b *recordingSMTP) NewSession(*gosmtp.Conn) (gosmtp.Session, error) {
	return &recordingSMTPSession{backend: b}, nil
}

func (b *recordingSMTP) snapshot() ([]string, []string, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.messages...), append([]string(nil), b.recipients...), b.from
}

type recordingSMTPSession struct{ backend *recordingSMTP }

func (s *recordingSMTPSession) Reset()        {}
func (s *recordingSMTPSession) Logout() error { return nil }

func (s *recordingSMTPSession) Mail(from string, _ *gosmtp.MailOptions) error {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	s.backend.from = from
	return s.backend.rejectWith
}

func (s *recordingSMTPSession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	s.backend.recipients = append(s.backend.recipients, to)
	return nil
}

// LOGIN is the mechanism the client picks for a password account, and go-sasl
// ships no server for it, so the two-step exchange is spelled out here: the
// username arrives as the initial response and the server answers "Password:".
func (s *recordingSMTPSession) AuthMechanisms() []string { return []string{"LOGIN"} }

func (s *recordingSMTPSession) Auth(string) (sasl.Server, error) {
	return &loginServer{backend: s.backend}, nil
}

type loginServer struct {
	backend *recordingSMTP
	step    int
}

func (l *loginServer) Next(response []byte) ([]byte, bool, error) {
	l.step++
	if l.step == 1 {
		l.backend.mu.Lock()
		l.backend.authUser = string(response)
		l.backend.mu.Unlock()
		return []byte("Password:"), false, nil
	}
	return nil, true, nil
}

func (s *recordingSMTPSession) Data(reader io.Reader) error {
	body, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	s.backend.mu.Lock()
	s.backend.messages = append(s.backend.messages, string(body))
	s.backend.mu.Unlock()
	return nil
}

// fullstack is a running application: a real IMAP server, a real SMTP server, the
// supervisor, every service, and the HTTP router in front of them.
type fullstack struct {
	*harness
	router http.Handler
	smtp   *recordingSMTP
	sender *sendservice.Worker
	cancel context.CancelFunc
}

func newFullstack(t *testing.T) *fullstack {
	t.Helper()
	h := newHarness(t)

	// A Sent folder so the post-delivery APPEND has somewhere to go: the qq preset
	// has ServerSavesSent false, which is what makes the append happen at all.
	if err := h.user.Create("Sent", nil); err != nil {
		t.Fatal(err)
	}

	// STARTTLS with a private CA rather than a plaintext server: the schema only
	// admits 'implicit' and 'starttls', and running the encrypted path is what the
	// real deployment does anyway.
	certificate, roots := loopbackCert(t)
	backend := &recordingSMTP{}
	smtpServer := gosmtp.NewServer(backend)
	smtpServer.Domain = "localhost"
	smtpServer.TLSConfig = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = smtpServer.Serve(listener) }()
	t.Cleanup(func() { _ = smtpServer.Close() })

	// The account was created from the qq preset, so its SMTP endpoint points at
	// smtp.qq.com. Repointing it at the loopback server is the only way to exercise
	// the send path end to end without touching the compiled-in presets.
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address %T", listener.Addr())
	}
	repointSMTP(t, h, "localhost", address.Port, "starttls")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	blobs := h.supervisor.blobs
	messages := messageservice.New(h.repo, h.supervisor, h.events)
	drafts := draftservice.New(h.repo, h.events, h.supervisor)
	t.Cleanup(drafts.Close)
	sessions := sessionservice.New(h.repo, fullstackAPIKey, time.Hour, 24*time.Hour)
	// 45s in production; a loopback conversation that takes 10s is broken.
	sender := sendservice.New(h.repo, blobs, h.accounts, nil, smtpprovider.NewWithRoots(10*time.Second, roots), h.events, 1<<20, h.supervisor)
	cfg := config.Config{PublicURL: "http://localhost:13737", APIKey: fullstackAPIKey, MaxOutboundBytes: 1 << 20}
	// The transport's hub only serves the websocket endpoint, which these tests do
	// not use; the events they assert on go to the recorder the supervisor and the
	// services publish into.
	api := httptransport.New(cfg, h.repo, blobs, h.accounts, messages, drafts, sessions, nil, h.supervisor, sender, realtime.New(), ctx)

	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.supervisor.Stop)
	go sender.Start(ctx)

	stack := &fullstack{harness: h, router: api.Handler(), smtp: backend, sender: sender, cancel: cancel}
	waitConnected(t, h)
	return stack
}

// repointSMTP rewrites the account's SMTP endpoint in place. There is no
// repository method for it — the endpoint comes from the provider preset and is
// never user editable — so the test writes the column directly.
func repointSMTP(t *testing.T, h *harness, host string, port int, mode string) {
	t.Helper()
	database, err := sql.Open("sqlite3", "file:"+h.dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open database for SMTP repoint: %v", err)
	}
	defer database.Close()
	if _, err := database.Exec(
		`UPDATE accounts SET smtp_host = ?, smtp_port = ?, smtp_tls_mode = ? WHERE id = ?`,
		host, port, mode, h.account.ID); err != nil {
		t.Fatalf("repoint SMTP: %v", err)
	}
	account, err := h.repo.GetAccount(context.Background(), h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if account.SMTPHost != host || account.SMTPPort != port {
		t.Fatalf("SMTP endpoint is still %s:%d", account.SMTPHost, account.SMTPPort)
	}
}

// loopbackCert issues a certificate for localhost and the pool that trusts it, so
// the send path runs its real TLS verification instead of being told to skip it.
func loopbackCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
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

// call issues an authenticated request through the whole middleware chain.
func (f *fullstack) call(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(payload)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("X-API-Key", fullstackAPIKey)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	return response
}

func decodeInto[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	return value
}

// TestFullStackReceiveReadAndSend is the acceptance path: mail arrives on the
// provider, the client lists it, opens it, replies, and the reply leaves over
// SMTP and lands in both the local database and the remote Sent folder.
func TestFullStackReceiveReadAndSend(t *testing.T) {
	stack := newFullstack(t)

	// 1. A session, because a browser client has no API key.
	login := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session",
		strings.NewReader(`{"api_key":"`+fullstackAPIKey+`"}`))
	login.Header.Set("Content-Type", "application/json")
	loginResponse := httptest.NewRecorder()
	stack.router.ServeHTTP(loginResponse, login)
	if loginResponse.Code != http.StatusCreated {
		t.Fatalf("login = %d: %s", loginResponse.Code, loginResponse.Body.String())
	}
	var cookie string
	for _, item := range loginResponse.Result().Cookies() {
		if item.Name == sessionservice.CookieName {
			cookie = item.Value
		}
	}
	if cookie == "" {
		t.Fatal("login returned no session cookie")
	}

	// The cookie is enough to reach a GET; the non-idempotent half of the
	// cookie channel is covered by the transport suite, which can forge the CSRF
	// token it needs. Here the point is only that a session issued by the real
	// login endpoint authenticates against the real router.
	cookieRead := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
	cookieRead.AddCookie(&http.Cookie{Name: sessionservice.CookieName, Value: cookie})
	cookieResponse := httptest.NewRecorder()
	stack.router.ServeHTTP(cookieResponse, cookieRead)
	if cookieResponse.Code != http.StatusOK {
		t.Fatalf("GET with the session cookie = %d: %s", cookieResponse.Code, cookieResponse.Body.String())
	}

	// 2. Mail lands on the IMAP server and the supervisor picks it up.
	stack.deliver(t, "fullstack-subject")
	stack.events.await(t, "NEW_EMAIL", 30*time.Second)

	// 3. The client lists its feed and sees it.
	var listed ports.MessagePage
	waitFor(t, 20*time.Second, func() bool {
		response := stack.call(t, http.MethodGet, "/api/v1/messages?limit=20", nil)
		if response.Code != http.StatusOK {
			return false
		}
		listed = decodeInto[ports.MessagePage](t, response)
		return len(listed.Items) > 0
	})
	found := false
	for _, item := range listed.Items {
		if item.Subject == "fullstack-subject" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the delivered message is not in the feed: %+v", listed.Items)
	}
	messageID := listed.Items[0].ID

	// 4. Opening it returns the body, fetched from IMAP if it was not prefetched.
	var detail messageDetail
	waitFor(t, 40*time.Second, func() bool {
		response := stack.call(t, http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", messageID), nil)
		if response.Code != http.StatusOK && response.Code != http.StatusAccepted {
			t.Fatalf("GET message = %d: %s", response.Code, response.Body.String())
		}
		detail = decodeInto[messageDetail](t, response)
		return detail.Message.BodyState == "ready"
	})
	if !strings.Contains(detail.Message.BodyText, "body of fullstack-subject") {
		t.Fatalf("body_text = %q, want the message body fetched over IMAP", detail.Message.BodyText)
	}

	// 5. Marking it read must reach the provider, not just the local row: a second
	// client has to see the \Seen flag.
	patch := stack.call(t, http.MethodPatch, fmt.Sprintf("/api/v1/messages/%d", messageID),
		map[string]any{"is_read": true})
	if patch.Code != http.StatusOK {
		t.Fatalf("PATCH message = %d: %s", patch.Code, patch.Body.String())
	}
	if !stack.remoteSeen(t, "INBOX") {
		t.Fatal("the message is not \\Seen on the provider after a read patch")
	}

	// 6. Compose a reply and queue it. The session cookie plus CSRF path is used
	// here so the browser-facing half of authenticate() is exercised too.
	created := stack.call(t, http.MethodPost, "/api/v1/drafts", map[string]any{
		"account_id": stack.account.ID,
		"to":         []string{"peer@example.com"},
		"subject":    "fullstack reply",
		"body_text":  "sent through the whole stack",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("POST draft = %d: %s", created.Code, created.Body.String())
	}
	draft := decodeInto[domain.Draft](t, created)
	if draft.ID == 0 {
		t.Fatalf("draft has no id: %s", created.Body.String())
	}

	send := stack.call(t, http.MethodPost, fmt.Sprintf("/api/v1/drafts/%d/send", draft.ID), nil)
	if send.Code != http.StatusAccepted {
		t.Fatalf("POST send = %d: %s", send.Code, send.Body.String())
	}

	// 7. The SMTP server really received it, addressed to the real recipient.
	waitFor(t, 30*time.Second, func() bool {
		messages, _, _ := stack.smtp.snapshot()
		return len(messages) > 0
	})
	messages, recipients, from := stack.smtp.snapshot()
	if from != stack.account.Email {
		t.Fatalf("MAIL FROM = %q, want %q", from, stack.account.Email)
	}
	if len(recipients) != 1 || recipients[0] != "peer@example.com" {
		t.Fatalf("recipients = %v", recipients)
	}
	if !strings.Contains(messages[0], "Subject: fullstack reply") ||
		!strings.Contains(messages[0], "sent through the whole stack") {
		t.Fatalf("the transmitted message is not the draft:\n%s", messages[0])
	}

	// 8. The draft became a sent message locally.
	waitFor(t, 30*time.Second, func() bool {
		response := stack.call(t, http.MethodGet, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), nil)
		if response.Code != http.StatusOK {
			return false
		}
		return decodeInto[draftDetail](t, response).Draft.Status == "sent"
	})
	final := decodeInto[draftDetail](t, stack.call(t, http.MethodGet, fmt.Sprintf("/api/v1/drafts/%d", draft.ID), nil))
	if final.Draft.LastError != nil {
		t.Fatalf("the sent draft carries an error: %q", *final.Draft.LastError)
	}
	outgoing := stack.messagesWithSubject(t, "fullstack reply")
	if len(outgoing) != 1 {
		t.Fatalf("%d local copies of the sent message, want 1", len(outgoing))
	}
	if outgoing[0].Direction != "outgoing" || !outgoing[0].IsRead {
		t.Fatalf("sent copy = %+v, want an outgoing read message", outgoing[0])
	}

	// 9. And it was appended to the provider's Sent folder, because the qq preset
	// says the server does not file it. This is the assertion that fails when the
	// ServerSavesSent branch regresses in either direction.
	waitFor(t, 30*time.Second, func() bool {
		return len(stack.remoteSubjects(t, "Sent")) > 0
	})
	subjects := stack.remoteSubjects(t, "Sent")
	if len(subjects) != 1 || !strings.Contains(subjects[0], "fullstack reply") {
		t.Fatalf("remote Sent holds %v, want exactly the one reply", subjects)
	}
}

// messagesWithSubject reads back through the repository rather than the API so the
// assertion is about what was persisted, not about what a filter returned.
func (f *fullstack) messagesWithSubject(t *testing.T, subject string) []domain.Message {
	t.Helper()
	page, err := f.repo.ListMessages(context.Background(), ports.MessageFilter{AccountID: &f.account.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var found []domain.Message
	for _, item := range page.Items {
		if item.Subject == subject {
			found = append(found, item)
		}
	}
	return found
}

// remoteSubjects lists what the IMAP server holds in a mailbox, read with an
// independent client so the supervisor's own view cannot mask a missing append.
func (f *fullstack) remoteSubjects(t *testing.T, mailbox string) []string {
	t.Helper()
	client := f.connect(t, context.Background())
	defer client.Close()
	data, err := client.Select(mailbox, nil).Wait()
	if err != nil {
		t.Fatalf("select %s: %v", mailbox, err)
	}
	if data.NumMessages == 0 {
		return nil
	}
	sequence := goimap.SeqSet{}
	sequence.AddRange(1, data.NumMessages)
	buffers, err := client.Fetch(sequence, &goimap.FetchOptions{Envelope: true}).Collect()
	if err != nil {
		t.Fatalf("fetch %s: %v", mailbox, err)
	}
	subjects := make([]string, 0, len(buffers))
	for _, buffer := range buffers {
		if buffer.Envelope != nil {
			subjects = append(subjects, buffer.Envelope.Subject)
		}
	}
	return subjects
}

// remoteSeen reports whether every message in a mailbox carries \Seen on the
// provider. A flag patch that only wrote the local row would leave this false.
func (f *fullstack) remoteSeen(t *testing.T, mailbox string) bool {
	t.Helper()
	client := f.connect(t, context.Background())
	defer client.Close()
	data, err := client.Select(mailbox, nil).Wait()
	if err != nil {
		t.Fatalf("select %s: %v", mailbox, err)
	}
	if data.NumMessages == 0 {
		return false
	}
	sequence := goimap.SeqSet{}
	sequence.AddRange(1, data.NumMessages)
	buffers, err := client.Fetch(sequence, &goimap.FetchOptions{Flags: true}).Collect()
	if err != nil {
		t.Fatalf("fetch flags: %v", err)
	}
	for _, buffer := range buffers {
		seen := false
		for _, flag := range buffer.Flags {
			if flag == goimap.FlagSeen {
				seen = true
			}
		}
		if !seen {
			return false
		}
	}
	return true
}

// containsDecoded reports whether the payload appears in the message once the
// transfer encoding is undone. Searching the raw text would miss it — base64 is
// the only encoding that can carry NUL and high bytes — and searching for the
// encoded form would pass on a truncated attachment, because a prefix of base64
// output is still a substring.
func containsDecoded(message string, payload []byte) bool {
	// Runs of consecutive base64 lines, so a MIME boundary or a header on either
	// side of the part does not end up inside the string being decoded.
	var run strings.Builder
	tryRun := func() bool {
		if run.Len() == 0 {
			return false
		}
		decoded, err := base64.StdEncoding.DecodeString(run.String())
		run.Reset()
		return err == nil && bytes.Contains(decoded, payload)
	}
	for _, line := range strings.Split(message, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if trimmed != "" && strings.Trim(trimmed, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=") == "" {
			run.WriteString(trimmed)
			continue
		}
		if tryRun() {
			return true
		}
	}
	return tryRun()
}

// messageDetail and draftDetail mirror the response envelopes the handlers write.
type messageDetail struct {
	Message domain.Message `json:"message"`
}
type draftDetail struct {
	Draft domain.Draft `json:"draft"`
}

// TestFullStackAttachmentRoundTrip covers the binary path: an upload becomes a
// blob, the blob becomes a MIME part on the wire, and the part survives transfer
// encoding byte for byte.
func TestFullStackAttachmentRoundTrip(t *testing.T) {
	stack := newFullstack(t)

	created := stack.call(t, http.MethodPost, "/api/v1/drafts", map[string]any{
		"account_id": stack.account.ID,
		"to":         []string{"peer@example.com"},
		"subject":    "with attachment",
		"body_text":  "see attached",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("POST draft = %d: %s", created.Code, created.Body.String())
	}
	draft := decodeInto[domain.Draft](t, created)

	// Bytes that a text-safe encoder would corrupt: NUL, high bytes, CRLF and a
	// bare dot on its own line, which is the SMTP terminator.
	payload := []byte("binary\x00\xff\xfe\r\n.\r\nafter the dot\r\n")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	upload := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/drafts/%d/attachments", draft.ID), bytes.NewReader(body.Bytes()))
	upload.Header.Set("Content-Type", writer.FormDataContentType())
	upload.Header.Set("X-API-Key", fullstackAPIKey)
	uploaded := httptest.NewRecorder()
	stack.router.ServeHTTP(uploaded, upload)
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", uploaded.Code, uploaded.Body.String())
	}

	if send := stack.call(t, http.MethodPost, fmt.Sprintf("/api/v1/drafts/%d/send", draft.ID), nil); send.Code != http.StatusAccepted {
		t.Fatalf("send = %d: %s", send.Code, send.Body.String())
	}
	waitFor(t, 30*time.Second, func() bool {
		messages, _, _ := stack.smtp.snapshot()
		return len(messages) > 0
	})
	messages, _, _ := stack.smtp.snapshot()
	// base64 is the only encoding that can carry those bytes, and the decoded part
	// has to match exactly. Asserting on the encoded form would pass even if the
	// payload were truncated.
	if !strings.Contains(messages[0], "payload.bin") {
		t.Fatalf("the attachment filename is missing from the message:\n%s", messages[0])
	}
	if !containsDecoded(messages[0], payload) {
		t.Fatalf("the attachment bytes did not survive the round trip:\n%s", messages[0])
	}
}

// TestFullStackSurvivesConcurrentClients drives the assembled stack the way a
// browser plus a couple of API integrations would: readers, a mark-read sweep and
// drafts all at once, while the supervisor keeps syncing new mail underneath.
func TestFullStackSurvivesConcurrentClients(t *testing.T) {
	stack := newFullstack(t)
	for i := range 12 {
		stack.deliver(t, fmt.Sprintf("concurrent-%02d", i))
	}
	waitFor(t, 40*time.Second, func() bool {
		page, err := stack.repo.ListMessages(context.Background(), ports.MessageFilter{AccountID: &stack.account.ID, Limit: 50})
		return err == nil && len(page.Items) >= 12
	})

	const clients = 12
	var wg sync.WaitGroup
	failures := make(chan string, clients*8)
	for index := range clients {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			for round := range 4 {
				switch (index + round) % 4 {
				case 0:
					if response := stack.call(t, http.MethodGet, "/api/v1/messages?limit=10", nil); response.Code != http.StatusOK {
						failures <- fmt.Sprintf("list = %d", response.Code)
					}
				case 1:
					if response := stack.call(t, http.MethodGet, "/api/v1/accounts", nil); response.Code != http.StatusOK {
						failures <- fmt.Sprintf("accounts = %d", response.Code)
					}
				case 2:
					response := stack.call(t, http.MethodPost, "/api/v1/drafts", map[string]any{
						"account_id": stack.account.ID,
						"to":         []string{"peer@example.com"},
						"subject":    fmt.Sprintf("draft %d-%d", index, round),
						"body_text":  "concurrent",
					})
					if response.Code != http.StatusCreated {
						failures <- fmt.Sprintf("create draft = %d: %s", response.Code, response.Body.String())
					}
				case 3:
					response := stack.call(t, http.MethodPost, "/api/v1/messages/mark-read",
						map[string]any{"account_id": stack.account.ID})
					if response.Code != http.StatusOK && response.Code != http.StatusMultiStatus {
						failures <- fmt.Sprintf("mark-read = %d: %s", response.Code, response.Body.String())
					}
				}
			}
		}(index)
	}
	// New mail keeps arriving while the clients hammer the API, so the command
	// connection is contended from both sides at once.
	stopDelivering := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stopDelivering:
				return
			default:
			}
			stack.deliver(t, fmt.Sprintf("during-load-%02d", i))
			time.Sleep(150 * time.Millisecond)
		}
	}()
	wg.Wait()
	close(stopDelivering)
	close(failures)
	for message := range failures {
		t.Error(message)
	}

	// The account must still be healthy, and everything delivered must still be
	// reachable: contention may delay a sync but must never drop mail.
	account, err := stack.repo.GetAccount(context.Background(), stack.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "connected" {
		t.Fatalf("account status = %q after the load, want connected (last error: %v)", account.Status, account.LastError)
	}
	waitFor(t, 40*time.Second, func() bool {
		return len(stack.messagesWithSubject(t, "concurrent-00")) == 1
	})
	page, err := stack.repo.ListMessages(context.Background(), ports.MessageFilter{AccountID: &stack.account.ID, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, item := range page.Items {
		seen[item.Subject]++
	}
	for i := range 12 {
		subject := fmt.Sprintf("concurrent-%02d", i)
		if seen[subject] != 1 {
			t.Errorf("%s appears %d times, want exactly 1", subject, seen[subject])
		}
	}
}
