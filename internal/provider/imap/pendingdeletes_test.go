//go:build sqlite_fts5

package imap

import (
	"context"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
)

// noPendingDeletes decides whether a plain EXPUNGE is safe. EXPUNGE removes every
// \Deleted message in the mailbox, not just the one being archived, so on a
// provider without UIDPLUS this answer is what stands between archiving one
// message and finalising another client's pending deletes.
//
// The archive path's use of it is covered by TestArchiveWithoutMoveOrUIDPlus. The
// function itself was not, and it is worth pinning directly: the destructive
// direction fails with no error anywhere, and the two answers have very different
// costs. A wrong false only strands archived mail on the server, which the caller
// logs. A wrong true destroys mail another client could still recover.

// TestNoPendingDeletesOnACleanMailbox covers the empty-search answer, the one that
// permits the EXPUNGE.
func TestNoPendingDeletesOnACleanMailbox(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := h.connect(t, ctx)
	t.Cleanup(func() { _ = client.Close() })

	h.deliver(t, "nothing deleted here")
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	safe, err := noPendingDeletes(client)
	if err != nil {
		t.Fatalf("noPendingDeletes: %v", err)
	}
	if !safe {
		t.Error("a mailbox with no \\Deleted message was reported unsafe to expunge; archived mail would be left on the server for no reason")
	}
}

// TestNoPendingDeletesRefusesWhenAnotherDeleteIsPending is the destructive half. A
// message flagged \Deleted by another client must make this answer false, because
// the EXPUNGE it would otherwise permit destroys that message too.
func TestNoPendingDeletesRefusesWhenAnotherDeleteIsPending(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := h.connect(t, ctx)
	t.Cleanup(func() { _ = client.Close() })

	h.deliver(t, "another client deleted me")
	h.deliver(t, "the one being archived")
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	// Flag one message the way another client would: \Deleted set, not yet
	// expunged, so the mail is still recoverable there.
	if _, err := client.Store(goimap.SeqSetNum(1), &goimap.StoreFlags{
		Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagDeleted},
	}, nil).Collect(); err != nil {
		t.Fatal(err)
	}

	safe, err := noPendingDeletes(client)
	if err != nil {
		t.Fatalf("noPendingDeletes: %v", err)
	}
	if safe {
		t.Error("a pending \\Deleted message did not block the expunge: archiving would destroy another client's deletion, unrecoverably and with no error")
	}
}
