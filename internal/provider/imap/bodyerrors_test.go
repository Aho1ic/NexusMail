//go:build sqlite_fts5

package imap

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

var literalCount = regexp.MustCompile(`\{([0-9]+)\}\r\n$`)

// missingSectionProxy relays IMAP except for FETCH body-section replies, which it
// turns into a valid FETCH response carrying only the UID. This is the protocol
// shape a provider sends when it still finds the message but the stored MIME part id
// has ceased to exist after a rewrite. imapmemserver instead supplies a present,
// empty section, so a normal in-memory integration test cannot reach this case.
type missingSectionProxy struct {
	target   string
	listener net.Listener
}

func newMissingSectionProxy(t *testing.T, target string) *missingSectionProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &missingSectionProxy{target: target, listener: listener}
	go proxy.serve()
	t.Cleanup(func() { _ = listener.Close() })
	return proxy
}

func (p *missingSectionProxy) dial(ctx context.Context, _ domain.Account) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", p.listener.Addr().String())
}

func (p *missingSectionProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		go func() { _, _ = io.Copy(server, client); _ = server.Close(); _ = client.Close() }()
		go p.relayResponses(server, client)
	}
}

func (p *missingSectionProxy) relayResponses(server, client net.Conn) {
	defer func() { _ = server.Close(); _ = client.Close() }()
	reader := bufio.NewReader(server)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			if isBodySectionFetch(line) {
				if !p.omitBodySection(reader, client, line) {
					return
				}
			} else if _, writeErr := io.WriteString(client, line); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func isBodySectionFetch(line string) bool {
	return strings.HasPrefix(line, "* ") && strings.Contains(line, " FETCH ") && strings.Contains(line, "BODY[")
}

func (p *missingSectionProxy) omitBodySection(reader *bufio.Reader, client net.Conn, header string) bool {
	match := literalCount.FindStringSubmatch(header)
	if len(match) != 2 {
		return false
	}
	size, err := strconv.Atoi(match[1])
	if err != nil {
		return false
	}
	// The body literal is followed by the closing ')' of the FETCH list. Discarding
	// both and emitting the prefix through UID keeps the response syntactically valid
	// while making FindBodySection return nil.
	if _, err := io.CopyN(io.Discard, reader, int64(size)); err != nil {
		return false
	}
	if _, err := reader.ReadString('\n'); err != nil {
		return false
	}
	prefix, _, found := strings.Cut(header, " BODY[")
	if !found {
		return false
	}
	_, err = io.WriteString(client, prefix+")\r\n")
	return err == nil
}

// TestFetchAttachmentDoesNotCacheAMissingRemotePart models a race that IMAP makes
// normal: the local metadata says an attachment is part 99, but the provider has
// replaced the message with a different MIME shape before the user clicks it. The
// body section is omitted from a live FETCH response, not merely empty.
func TestFetchAttachmentDoesNotCacheAMissingRemotePart(t *testing.T) {
	h := newHarness(t)
	proxy := newMissingSectionProxy(t, serverAddress(t, h))
	h.supervisor.dial = proxy.dial
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.supervisor.Stop() })
	waitConnected(t, h)
	const raw = "MIME-Version: 1.0\r\nMessage-Id: <stale-part@example.com>\r\n" +
		"From: Sender <sender@example.com>\r\nTo: mail@example.com\r\nSubject: stale part\r\n" +
		"Content-Type: multipart/mixed; boundary=BOUND\r\n\r\n" +
		"--BOUND\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--BOUND\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=payload.bin\r\n\r\npayload\r\n" +
		"--BOUND--\r\n"
	if _, err := h.user.Append("INBOX", literal{strings.NewReader(raw)}, &goimap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatalf("append: %v", err)
	}
	messageID := waitForMessage(t, h)

	var attachment domain.Attachment
	waitFor(t, 60*time.Second, func() bool {
		_, attachments, err := h.repo.GetMessage(ctx, messageID)
		if err != nil || len(attachments) != 1 {
			return false
		}
		attachment = attachments[0]
		return true
	})

	database, err := sql.Open("sqlite3", "file:"+h.dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.ExecContext(ctx, "UPDATE attachments SET part_id = ? WHERE id = ?", "99", attachment.ID); err != nil {
		t.Fatalf("make local part id stale: %v", err)
	}

	_, fetched, err := h.supervisor.FetchAttachment(ctx, messageID, attachment.ID)
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("fetch stale part error = %v, want not found", err)
	}
	if fetched.BlobID != nil || fetched.FetchState == "ready" {
		t.Errorf("stale part was cached as %+v", fetched)
	}
	_, stored, err := h.repo.GetMessage(ctx, messageID)
	if err != nil {
		t.Fatal(err)
	}
	if stored[0].BlobID != nil || stored[0].FetchState == "ready" {
		t.Errorf("database attachment was marked ready after a missing part: %+v", stored[0])
	}
}

// TestFindBodySectionReportsAMissingPartAsNil pins the library contract the
// integration test above uses. A present zero-byte attachment is non-nil and must
// remain cacheable, while an omitted section is nil and is NotFound.
func TestFindBodySectionReportsAMissingPartAsNil(t *testing.T) {
	requested := &goimap.FetchItemBodySection{Part: []int{99}, Peek: true}
	present := &goimap.FetchItemBodySection{Part: []int{2}, Peek: true}
	buffer := &imapclient.FetchMessageBuffer{UID: 1, BodySection: []imapclient.FetchBodySectionBuffer{{Section: present, Bytes: []byte("payload")}}}
	if got := buffer.FindBodySection(requested); got != nil {
		t.Errorf("a section the server omitted returned %q, want nil", got)
	}
	if got := buffer.FindBodySection(present); string(got) != "payload" {
		t.Errorf("the present section returned %q, want payload", got)
	}
	empty := &goimap.FetchItemBodySection{Part: []int{3}, Peek: true}
	buffer.BodySection = append(buffer.BodySection, imapclient.FetchBodySectionBuffer{Section: empty, Bytes: []byte{}})
	if got := buffer.FindBodySection(empty); got == nil {
		t.Error("a present but empty section returned nil, so empty attachments would be rejected")
	}
}
