//go:build sqlite_fts5

package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

// MessageLocation is the lookup every remote operation starts from: fetching a
// body, fetching an attachment, and archiving all resolve a message to an account,
// a mailbox and a UID before touching the provider. Its not-found answer therefore
// has to be a classified NotFound, because that is what the HTTP layer turns into
// a 404 — an unclassified error becomes a 500, and a nil error with a zero value
// would send the supervisor to fetch UID 0 from an empty mailbox name.
//
// The batch sibling MessageLocations was already covered; the single-message
// function's miss paths were not.

// TestMessageLocationReportsNotFoundForAnUnmappedMessage covers a message row that
// exists but has no mailbox mapping — the state a message is left in after its
// last mailbox link is expunged, and the state a message created by the send path
// is in until it is linked.
func TestMessageLocationReportsNotFoundForAnUnmappedMessage(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, _ := seedAccountMailbox(t, store)

	// An outgoing message, which never gets a mailbox_messages row.
	now := time.Now().UnixMilli()
	message := domain.Message{
		AccountID: account.ID, Direction: "outgoing", DedupeKey: []byte("no-mapping-key-32-bytes-padding!"),
		Subject: "unmapped", Sender: "me@example.com", Recipients: "them@example.com",
		FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
		BodyState: "ready", ReceivedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.db.WithContext(ctx).Create(&message).Error; err != nil {
		t.Fatal(err)
	}

	location, err := store.MessageLocation(ctx, message.ID)
	if err == nil {
		t.Fatalf("MessageLocation returned no error for an unmapped message: %#v", location)
	}
	if !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("MessageLocation error = %v, want a NotFound: an unclassified error becomes a 500 instead of a 404", err)
	}
	if location.UID != 0 || location.MessageID != 0 {
		t.Errorf("MessageLocation returned a populated location alongside its error: %#v", location)
	}
}

// TestMessageLocationReportsNotFoundForAMissingMessage covers an id that names no
// row at all, which is what a stale browser tab sends after the mail was expunged.
func TestMessageLocationReportsNotFoundForAMissingMessage(t *testing.T) {
	store := openTestStore(t)
	seedAccountMailbox(t, store)

	_, err := store.MessageLocation(context.Background(), 999_999)
	if !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("MessageLocation(999999) error = %v, want a NotFound", err)
	}
}

// TestMessageLocationResolvesAMappedMessage is the positive half, and pins the
// fields the supervisor actually reads. Returning the mailbox's RemoteName rather
// than its display name is what makes the following SELECT valid.
func TestMessageLocationResolvesAMappedMessage(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	messageID := seedMessage(t, store, account.ID, mailbox.ID, 77, "mapped", "incoming", false, time.Now().UnixMilli())

	location, err := store.MessageLocation(ctx, messageID)
	if err != nil {
		t.Fatalf("MessageLocation: %v", err)
	}
	if location.MessageID != messageID {
		t.Errorf("MessageID = %d, want %d", location.MessageID, messageID)
	}
	if location.UID != 77 {
		t.Errorf("UID = %d, want 77: the supervisor would fetch the wrong message", location.UID)
	}
	if location.Account.ID != account.ID {
		t.Errorf("Account.ID = %d, want %d", location.Account.ID, account.ID)
	}
	if location.Mailbox.ID != mailbox.ID {
		t.Errorf("Mailbox.ID = %d, want %d", location.Mailbox.ID, mailbox.ID)
	}
	if location.Mailbox.RemoteName != "INBOX" {
		t.Errorf("Mailbox.RemoteName = %q, want INBOX: SELECT takes the remote name, not the display name", location.Mailbox.RemoteName)
	}
}

// TestMessageLocationPrefersInboxOverOtherMailboxes pins the ORDER BY. A message
// mapped into two mailboxes — the copy a provider leaves in All Mail alongside
// INBOX — must resolve to the inbox copy, because that is the one the feed shows
// and the one a flag change is expected to affect.
func TestMessageLocationPrefersInboxOverOtherMailboxes(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, inbox := seedAccountMailbox(t, store)

	now := time.Now().UnixMilli()
	other := domain.Mailbox{
		AccountID: account.ID, RemoteName: "All Mail", DisplayName: "All Mail",
		Role: "all", SyncMode: "lazy", UIDValidity: 42, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.UpsertMailbox(ctx, &other); err != nil {
		t.Fatal(err)
	}
	// UpsertMailbox does not fill in the id, so read it back the way
	// seedAccountMailbox does.
	otherID := int64(0)
	mailboxes, err := store.ListMailboxes(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range mailboxes {
		if candidate.RemoteName == "All Mail" {
			otherID = candidate.ID
		}
	}
	if otherID == 0 {
		t.Fatalf("All Mail was not stored: %#v", mailboxes)
	}

	// Link the archive copy first so insertion order cannot be what makes the
	// assertion pass.
	messageID := seedMessage(t, store, account.ID, otherID, 900, "two copies", "incoming", false, now)
	linkMessage(t, store, account.ID, inbox.ID, 5, "two copies", "incoming", false, now)

	location, locErr := store.MessageLocation(ctx, messageID)
	if locErr != nil {
		t.Fatalf("MessageLocation: %v", locErr)
	}
	if location.Mailbox.ID != inbox.ID {
		t.Errorf("resolved mailbox %d, want the inbox %d: a flag change would land on the wrong copy", location.Mailbox.ID, inbox.ID)
	}
	if location.UID != 5 {
		t.Errorf("UID = %d, want the inbox UID 5", location.UID)
	}
}
