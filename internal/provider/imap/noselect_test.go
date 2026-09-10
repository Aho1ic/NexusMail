//go:build sqlite_fts5

package imap

import (
	"bufio"
	"context"
	"io"
	"net"
	"slices"
	"strings"
	"testing"

	"nexusmail/internal/domain"

	goimap "github.com/emersion/go-imap/v2"
)

// noselectContainer is the container name QQ advertises. It is the folder the
// original report came from: it appeared in the sidebar, answered
// "NO Folder not exist!" on SELECT, and any message MessageLocation resolved to it
// could never be flagged.
const noselectContainer = "其他文件夹"

// noselectProxy relays IMAP and injects one \Noselect LIST row into the catalog
// reply. imapmemserver only ever creates selectable mailboxes, so a container is
// unreachable without shaping the protocol directly — the same reason
// missingSectionProxy exists for body sections.
type noselectProxy struct {
	target   string
	listener net.Listener
}

func newNoselectProxy(t *testing.T, target string) *noselectProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &noselectProxy{target: target, listener: listener}
	go proxy.serve()
	t.Cleanup(func() { _ = listener.Close() })
	return proxy
}

func (p *noselectProxy) dial(ctx context.Context, _ domain.Account) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", p.listener.Addr().String())
}

func (p *noselectProxy) serve() {
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

// relayResponses copies the server's stream through, prepending the container row
// to the first LIST row it sees. Injecting before rather than after keeps the
// tagged completion line last, which is what the client waits on.
func (p *noselectProxy) relayResponses(server, client net.Conn) {
	defer func() { _ = server.Close(); _ = client.Close() }()
	reader := bufio.NewReader(server)
	injected := false
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			if !injected && strings.HasPrefix(line, "* LIST ") {
				injected = true
				row := "* LIST (\\Noselect \\HasChildren) \"/\" \"" + noselectContainer + "\"\r\n"
				if _, writeErr := io.WriteString(client, row); writeErr != nil {
					return
				}
			}
			if _, writeErr := io.WriteString(client, line); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// TestCatalogDoesNotStoreANoselectContainer is the regression for the reported
// defect. The catalog used to upsert every LIST row, so a container the provider
// declares unselectable became a mailbox row like any other. Three things broke
// from that one write, and only the third was visible to the user:
//
//   - it was offered in the sidebar and failed with the provider's own
//     "Folder not exist!" when opened;
//   - the periodic pass tried to SELECT it;
//   - MessageLocation could resolve a message to it, so SetFlags answered 500 for
//     that message forever, while the browser had already drawn the row read —
//     which is exactly "mail I read goes back to unread after refreshing".
func TestCatalogDoesNotStoreANoselectContainer(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The proxy is spliced in front of the memory server so the LIST reply carries a
	// container the server itself cannot produce.
	proxy := newNoselectProxy(t, h.serverAddr)
	h.supervisor.dial = proxy.dial

	rt := &runtime{account: h.account, syncReq: make(chan int64, 8)}
	client := h.connect(t, ctx)
	t.Cleanup(func() { _ = client.Close() })

	items, err := h.supervisor.refreshMailboxCatalog(ctx, rt, client)
	if err != nil {
		t.Fatalf("refresh mailbox catalog: %v", err)
	}

	// The row must still reach the caller: ensureArchiveMailbox looks for exactly
	// these containers to nest a created archive folder under, and it reads the
	// returned LIST entries rather than the database.
	listed := false
	for _, item := range items {
		if item.Mailbox != noselectContainer {
			continue
		}
		listed = true
		if !isNoselect(item) {
			t.Fatalf("the injected row lost its attributes: %v", item.Attrs)
		}
	}
	if !listed {
		t.Fatal("the container was filtered out of the returned LIST entries, so archivePaths can no longer nest an archive under it")
	}

	stored, err := h.repo.ListMailboxes(ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(stored))
	for _, mailbox := range stored {
		names = append(names, mailbox.RemoteName)
		if mailbox.RemoteName == noselectContainer {
			t.Errorf("the \\Noselect container was stored as a mailbox (role=%q mode=%q); every path that resolves it fails against the provider",
				mailbox.Role, mailbox.SyncMode)
		}
	}
	// The filter must be narrow: the selectable mailboxes are still the catalog.
	if !slices.Contains(names, "INBOX") {
		t.Errorf("stored mailboxes = %v, want INBOX among them", names)
	}
}

// TestIsNoselectReadsTheAttributeOnly pins the predicate both callers share. A
// looser test — the name, or the presence of children — would let a folder the user
// created for their own mail be treated as a container, which drops their mail out
// of the catalog entirely.
func TestIsNoselectReadsTheAttributeOnly(t *testing.T) {
	for _, item := range []*goimap.ListData{
		listItem(noselectContainer, '/', goimap.MailboxAttrNoSelect),
		listItem("[Gmail]", '/', goimap.MailboxAttrNoSelect, goimap.MailboxAttrHasChildren),
		listItem("Flat", 0, goimap.MailboxAttrNoSelect),
	} {
		if !isNoselect(item) {
			t.Errorf("isNoselect(%q) = false, want true", item.Mailbox)
		}
	}
	for _, item := range []*goimap.ListData{
		listItem("INBOX", '/'),
		// A selectable parent: it holds children and mail of its own.
		listItem("Projects", '/', goimap.MailboxAttrHasChildren),
		// Named like a container but never declared one.
		listItem(noselectContainer, '/'),
	} {
		if isNoselect(item) {
			t.Errorf("isNoselect(%q) = true, want false", item.Mailbox)
		}
	}
}
