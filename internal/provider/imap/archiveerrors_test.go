//go:build sqlite_fts5

package imap

import (
	"context"
	"errors"
	"testing"
)

// TestEnsureArchiveMailboxPropagatesLookupErrors verifies that only a genuine
// NotFound starts a remote LIST/CREATE attempt. Treating every repository failure
// as "the role is absent" turns cancellation or a closed database into network work
// (and can create a folder after the caller has already given up). A nil client is
// deliberate: a cancelled lookup must return before any IMAP operation is reached.
func TestEnsureArchiveMailboxPropagatesLookupErrors(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := h.supervisor.ensureArchiveMailbox(ctx, &runtime{account: h.account}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled role lookup returned %v, want context.Canceled", err)
	}
}
