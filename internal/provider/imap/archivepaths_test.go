//go:build sqlite_fts5

package imap

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"

	goimap "github.com/emersion/go-imap/v2"
)

// listItem builds one LIST reply row. The fields archivePaths reads are the
// delimiter and the attributes, so those are what this exposes.
func listItem(name string, delim rune, attrs ...goimap.MailboxAttr) *goimap.ListData {
	return &goimap.ListData{Mailbox: name, Delim: delim, Attrs: attrs}
}

// TestArchivePathsPrefersRootThenNoselectContainers pins the candidate order and,
// more importantly, which containers are eligible. The attribute test is not a
// detail: \Noselect is the server's own statement that a folder holds only
// children, and using anything looser would let a top-level folder the user
// created for their own mail become the archive's parent.
func TestArchivePathsPrefersRootThenNoselectContainers(t *testing.T) {
	items := []*goimap.ListData{
		// A selectable folder the user made. Never a parent, even though it has a
		// delimiter and could hold children.
		listItem("Projects", '/'),
		listItem("其他文件夹", '/', goimap.MailboxAttrNoSelect),
		// No delimiter means the server exposes no hierarchy under it, so there is no
		// path to build even though it is a container.
		listItem("Flat", 0, goimap.MailboxAttrNoSelect),
		listItem("[Gmail]", '/', goimap.MailboxAttrNoSelect, goimap.MailboxAttrHasChildren),
	}
	got := archivePaths("Archive", items)
	want := []string{"Archive", "其他文件夹/Archive", "[Gmail]/Archive"}
	if len(got) != len(want) {
		t.Fatalf("archivePaths = %q, want %q", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("candidate %d = %q, want %q", index, got[index], want[index])
		}
	}
	// The root-level name has to come first: on a provider that allows it, that is
	// where a real sibling of INBOX belongs, and trying a container first would bury
	// the folder for no reason.
	if got[0] != "Archive" {
		t.Errorf("first candidate = %q, want the root-level name", got[0])
	}
}

// TestArchivePathsUsesTheServersDelimiter covers the separator coming from LIST
// rather than a hardcoded "/". A provider using "." would otherwise get a folder
// literally named "Container/Archive" instead of a child of Container.
func TestArchivePathsUsesTheServersDelimiter(t *testing.T) {
	got := archivePaths("Archives", []*goimap.ListData{
		listItem("INBOX", '.', goimap.MailboxAttrNoSelect),
	})
	want := []string{"Archives", "INBOX.Archives"}
	for index := range want {
		if index >= len(got) || got[index] != want[index] {
			t.Fatalf("archivePaths = %q, want %q", got, want)
		}
	}
}

// TestArchivePathsWithNoContainers is the QQ and 163 shape: no \Noselect folder is
// advertised, so the root-level name is the only candidate. It must still be
// offered, otherwise those accounts get no archive at all — the original bug.
func TestArchivePathsWithNoContainers(t *testing.T) {
	for name, items := range map[string][]*goimap.ListData{
		"empty listing":     nil,
		"only selectable":   {listItem("INBOX", '/'), listItem("Sent", '/')},
		"no delimiter":      {listItem("INBOX", 0, goimap.MailboxAttrNoSelect)},
		"selectable parent": {listItem("Mine", '/', goimap.MailboxAttrHasChildren)},
	} {
		got := archivePaths("Archive", items)
		if len(got) != 1 || got[0] != "Archive" {
			t.Errorf("%s: archivePaths = %q, want just [Archive]", name, got)
		}
	}
}

// withCreateSpecialUse advertises CREATE-SPECIAL-USE. Neither IMAP4rev1 nor rev2
// implies it, so without this option archiveCreateOptions can only ever be
// exercised on its nil branch.
func withCreateSpecialUse() harnessOption {
	return func(cfg *harnessConfig) {
		cfg.caps = goimap.CapSet{
			goimap.CapIMAP4rev1: {}, goimap.CapIMAP4rev2: {},
			goimap.Cap("CREATE-SPECIAL-USE"): {},
		}
	}
}

// TestArchiveCreateOptionsFollowTheAdvertisedCapability pins both branches against
// a real client's capability set. Sending SPECIAL-USE to a server that never
// advertised it risks a rejected CREATE — which on QQ and 163 means no archive
// folder — and omitting it where it is supported loses the point of asking: other
// clients would not see the folder as the archive.
func TestArchiveCreateOptionsFollowTheAdvertisedCapability(t *testing.T) {
	ctx := context.Background()

	plain := newHarness(t)
	plainClient := plain.connect(t, ctx)
	defer plainClient.Close()
	if options := archiveCreateOptions(plainClient); options != nil {
		t.Errorf("options = %+v for a server without CREATE-SPECIAL-USE, want nil", options)
	}

	special := newHarness(t, withCreateSpecialUse())
	specialClient := special.connect(t, ctx)
	defer specialClient.Close()
	options := archiveCreateOptions(specialClient)
	if options == nil {
		t.Fatal("options = nil for a server advertising CREATE-SPECIAL-USE")
	}
	if len(options.SpecialUse) != 1 || options.SpecialUse[0] != goimap.MailboxAttrArchive {
		t.Errorf("SpecialUse = %v, want [%v]", options.SpecialUse, goimap.MailboxAttrArchive)
	}
}

// TestArchiveReportsEveryRejectedCandidate covers the path where the provider
// refuses to create the folder anywhere. Before this the only archive test was the
// happy path, so a failure returned whatever the last attempt happened to leave
// behind. The requirement is that the error is Unavailable — the transport layer
// maps that to a retryable status rather than a bug — and that it names the
// attempts, because the user's only recourse is to create the folder by hand and
// they need to know which names were tried.
func TestArchiveReportsEveryRejectedCandidate(t *testing.T) {
	h := newHarness(t)
	refuser := newCreateRefuser(t, serverAddress(t, h))
	h.supervisor.dial = refuser.dial

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.supervisor.Stop() })
	waitConnected(t, h)
	h.deliver(t, "cannot-archive")
	messageID := waitForMessage(t, h)

	err := h.supervisor.Archive(ctx, messageID)
	if err == nil {
		t.Fatal("archive succeeded even though every CREATE was refused")
	}
	if !errors.Is(err, ports.ErrUnavailable) {
		t.Errorf("error %v is not Unavailable, so the API would report it as a bug", err)
	}
	for _, name := range archiveCandidateNames {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention the %q attempt", err, name)
		}
	}
	// The message stays where it is. A failed archive that had already removed the
	// message from INBOX would lose it.
	client := h.connect(t, ctx)
	defer client.Close()
	if uids := remoteUIDs(t, client, "INBOX"); len(uids) != 1 {
		t.Errorf("INBOX holds %d messages after a failed archive, want 1", len(uids))
	}
}

// createRefuser relays IMAP but answers every CREATE with NO instead of passing it
// upstream, which is how a provider that only permits folders in places we did not
// guess behaves. A blackhole cannot model this: the point is a live connection that
// keeps working for everything except the one command.
type createRefuser struct {
	target   string
	listener net.Listener
}

func newCreateRefuser(t *testing.T, target string) *createRefuser {
	t.Helper()
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	refuser := &createRefuser{target: target, listener: listener}
	go refuser.serve()
	t.Cleanup(func() { _ = listener.Close() })
	return refuser
}

func (c *createRefuser) dial(ctx context.Context, _ domain.Account) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", c.listener.Addr().String())
}

func (c *createRefuser) serve() {
	for {
		downstream, err := c.listener.Accept()
		if err != nil {
			return
		}
		upstream, err := net.Dial("tcp", c.target)
		if err != nil {
			_ = downstream.Close()
			continue
		}
		// One mutex per connection pair guards writes towards the client, because the
		// injected NO and the relayed server output are produced by different
		// goroutines and must not interleave inside a line.
		var writeMu sync.Mutex
		go c.filterCommands(downstream, upstream, &writeMu)
		go c.relayResponses(upstream, downstream, &writeMu)
	}
}

// filterCommands reads client commands a line at a time so a CREATE can be answered
// locally. Everything else is forwarded verbatim, including literals, which arrive
// as further lines and are relayed like any other.
func (c *createRefuser) filterCommands(client, server net.Conn, writeMu *sync.Mutex) {
	defer func() { _ = client.Close(); _ = server.Close() }()
	reader := bufio.NewReader(client)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			if tag, ok := createTag(line); ok {
				writeMu.Lock()
				_, writeErr := client.Write([]byte(tag + " NO [CANNOT] folder creation is not permitted here\r\n"))
				writeMu.Unlock()
				if writeErr != nil {
					return
				}
				continue
			}
			if _, writeErr := server.Write([]byte(line)); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *createRefuser) relayResponses(server, client net.Conn, writeMu *sync.Mutex) {
	defer func() { _ = client.Close(); _ = server.Close() }()
	buffer := make([]byte, 4096)
	for {
		read, err := server.Read(buffer)
		if read > 0 {
			writeMu.Lock()
			_, writeErr := client.Write(buffer[:read])
			writeMu.Unlock()
			if writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// createTag returns the command tag when line is a CREATE. The tag is needed
// because an IMAP client matches a completion result to a command by tag and would
// ignore an untagged NO.
func createTag(line string) (string, bool) {
	fields := strings.Fields(strings.TrimRight(line, "\r\n"))
	if len(fields) < 2 || !strings.EqualFold(fields[1], "CREATE") {
		return "", false
	}
	return fields[0], true
}
