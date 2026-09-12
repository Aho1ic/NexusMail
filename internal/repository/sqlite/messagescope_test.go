//go:build sqlite_fts5

package sqlite

import (
	"context"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

// legacyScopedIDs is the query the feed used before applyMessageScope switched to
// EXISTS: JOIN mailbox_messages + mailboxes, deduplicated with SELECT DISTINCT.
// It is reproduced here so the rewrite is pinned against the behaviour it
// replaced rather than against a fresh set of hand-written expectations, which
// could encode the same mistake twice.
//
// The join was conditional in the original too — only a mailbox or folder filter
// added it — so an unscoped feed never required a mailbox mapping.
func legacyScopedIDs(t *testing.T, store *Store, filter ports.MessageFilter) []int64 {
	t.Helper()
	query := store.db.WithContext(context.Background()).Model(&domain.Message{})
	if filter.AccountID != nil {
		query = query.Where("messages.account_id = ?", *filter.AccountID)
	}
	if filter.MailboxID != nil || filter.Folder != "" {
		query = query.Joins("JOIN mailbox_messages mm ON mm.message_id = messages.id").
			Joins("JOIN mailboxes mb ON mb.id = mm.mailbox_id")
		if filter.MailboxID != nil {
			query = query.Where("mb.id = ?", *filter.MailboxID)
		}
		if filter.Folder != "" {
			query = query.Where("mb.role = ?", filter.Folder)
		}
	}
	if filter.IsRead != nil {
		query = query.Where("messages.is_read = ?", *filter.IsRead)
	}
	if filter.Query != "" {
		query = applyMessageSearch(query, filter.Query)
	}
	var ids []int64
	if err := query.Distinct().Order("messages.received_at DESC, messages.id DESC").
		Pluck("messages.id", &ids).Error; err != nil {
		t.Fatalf("legacy scoped query: %v", err)
	}
	return ids
}

func feedIDs(t *testing.T, store *Store, filter ports.MessageFilter) []int64 {
	t.Helper()
	filter.Limit = 100
	page, err := store.ListMessages(context.Background(), filter)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	ids := make([]int64, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

// scopeFixture files mail so that every branch of applyMessageScope has
// something to include and something to exclude, including the case the old
// DISTINCT existed for: one message mapped into two mailboxes.
type scopeFixture struct {
	account, other   domain.Account
	inbox, archive   domain.Mailbox
	otherInbox       domain.Mailbox
	inboxOnly        int64
	bothMailboxes    int64
	archiveOnly      int64
	readInbox        int64
	otherAccountMail int64
	unmapped         int64
}

func seedScopeFixture(t *testing.T, store *Store) scopeFixture {
	t.Helper()
	ctx := context.Background()
	account, inbox := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()
	archive := domain.Mailbox{AccountID: account.ID, RemoteName: "Archive", DisplayName: "Archive", Role: "archive", SyncMode: "lazy", UIDValidity: 7, CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertMailbox(ctx, &archive); err != nil {
		t.Fatal(err)
	}
	boxes, err := store.ListMailboxes(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, box := range boxes {
		if box.Role == "archive" {
			archive = box
		}
	}
	other, otherInbox := seedSecondAccount(t, store)

	fixture := scopeFixture{account: account, other: other, inbox: inbox, archive: archive, otherInbox: otherInbox}
	fixture.inboxOnly = seedMessage(t, store, account.ID, inbox.ID, 1, "inbox hello alpha", "incoming", false, now)
	fixture.bothMailboxes = seedMessage(t, store, account.ID, inbox.ID, 2, "filed twice hello beta", "incoming", false, now+1_000)
	// The same message in a second mailbox: two joined rows for one message once a
	// filter reaches both mailboxes.
	linkMessage(t, store, account.ID, archive.ID, 2, "filed twice hello beta", "incoming", false, now+1_000)
	// The same message twice inside ONE mailbox, under two UIDs. mailbox_messages
	// is keyed on (mailbox_id, uid), so this is a legal state and the one the feed's
	// DISTINCT was actually load-bearing for: a re-delivery or a UIDVALIDITY change
	// leaves a message reachable under a second UID, and a folder filter then
	// matched the join twice and listed the mail twice.
	linkMessage(t, store, account.ID, inbox.ID, 99, "filed twice hello beta", "incoming", false, now+1_000)
	fixture.archiveOnly = seedMessage(t, store, account.ID, archive.ID, 3, "archived hello gamma", "incoming", false, now+2_000)
	fixture.readInbox = seedMessage(t, store, account.ID, inbox.ID, 4, "already read hello delta", "incoming", true, now+3_000)
	fixture.otherAccountMail = seedMessage(t, store, other.ID, otherInbox.ID, 1, "other account hello epsilon", "incoming", false, now+4_000)

	// A message with no mailbox_messages row at all: the join dropped it and so
	// must EXISTS. CreateSentMessage is the production path that produces one.
	digest := []byte("unmapped-dedupe-key-000000000000")
	unmapped := domain.Message{
		AccountID: account.ID, Direction: "outgoing", DedupeKey: digest, Subject: "unmapped hello zeta",
		Sender: "me@example.com", Recipients: "you@example.com",
		FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
		BodyState: "metadata", ReceivedAt: now + 5_000, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.db.WithContext(ctx).Create(&unmapped).Error; err != nil {
		t.Fatal(err)
	}
	fixture.unmapped = unmapped.ID
	return fixture
}

// TestMessageScopeMatchesLegacyJoin is the equality check for the EXISTS rewrite:
// every filter combination the transport can build must select exactly the rows
// the JOIN + DISTINCT form selected.
func TestMessageScopeMatchesLegacyJoin(t *testing.T) {
	store := openTestStore(t)
	fixture := seedScopeFixture(t, store)
	unread := false

	cases := []struct {
		name   string
		filter ports.MessageFilter
	}{
		{"account + folder", ports.MessageFilter{AccountID: &fixture.account.ID, Folder: "inbox"}},
		{"account + mailbox", ports.MessageFilter{AccountID: &fixture.account.ID, MailboxID: &fixture.archive.ID}},
		{"folder only (every account)", ports.MessageFilter{Folder: "inbox"}},
		{"mailbox only", ports.MessageFilter{MailboxID: &fixture.inbox.ID}},
		{"mailbox + folder agreeing", ports.MessageFilter{MailboxID: &fixture.archive.ID, Folder: "archive"}},
		// mb.id and mb.role had to be satisfied by the same joined row, so a
		// mailbox named with a role it does not have selected nothing. An EXISTS
		// that split the two predicates across separate subqueries would wrongly
		// match here.
		{"mailbox + folder contradicting", ports.MessageFilter{MailboxID: &fixture.inbox.ID, Folder: "archive"}},
		{"query, FTS branch", ports.MessageFilter{Folder: "inbox", Query: "hello"}},
		{"query, LIKE branch", ports.MessageFilter{Folder: "inbox", Query: "be"}},
		{"account + mailbox + query + is_read", ports.MessageFilter{AccountID: &fixture.account.ID, MailboxID: &fixture.inbox.ID, Query: "hello", IsRead: &unread}},
		{"no scope at all", ports.MessageFilter{}},
		{"other account", ports.MessageFilter{AccountID: &fixture.other.ID}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			want := legacyScopedIDs(t, store, testCase.filter)
			got := feedIDs(t, store, testCase.filter)
			if !equalIDs(got, want) {
				t.Fatalf("feed = %v, legacy JOIN+DISTINCT = %v", got, want)
			}
		})
	}
}

// The unscoped feed is the one case where the two forms legitimately differ: the
// join required a mailbox mapping, so a message with none was invisible even
// with no filter, while EXISTS is only applied when a mailbox or folder is asked
// for. Sent mail written by CreateSentMessage has no mapping until the provider
// reports it in the Sent folder, so this is the behaviour worth pinning.
func TestUnscopedFeedIncludesUnmappedMail(t *testing.T) {
	store := openTestStore(t)
	fixture := seedScopeFixture(t, store)
	ids := feedIDs(t, store, ports.MessageFilter{})
	found := false
	for _, id := range ids {
		if id == fixture.unmapped {
			found = true
		}
	}
	if !found {
		t.Fatalf("unscoped feed %v omits the message with no mailbox mapping (%d)", ids, fixture.unmapped)
	}
	// Asking for a folder still requires the mapping.
	for _, id := range feedIDs(t, store, ports.MessageFilter{Folder: "inbox"}) {
		if id == fixture.unmapped {
			t.Fatal("folder=inbox returned a message with no mailbox mapping")
		}
	}
}

// The feed must not return a message twice now that DISTINCT is gone. The
// fixture's bothMailboxes row is filed in two mailboxes, which is exactly the
// shape that duplicated under a join.
func TestFeedDoesNotDuplicateMessageFiledTwice(t *testing.T) {
	store := openTestStore(t)
	fixture := seedScopeFixture(t, store)
	for _, filter := range []ports.MessageFilter{
		{},
		{AccountID: &fixture.account.ID},
		{Folder: "inbox"},
		{MailboxID: &fixture.inbox.ID},
		{AccountID: &fixture.account.ID, Query: "hello"},
	} {
		seen := map[int64]int{}
		for _, id := range feedIDs(t, store, filter) {
			seen[id]++
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("filter %+v returned message %d %d times", filter, id, n)
			}
		}
	}
}

// unreadTotal and UnreadMessageIDs share applyMessageScope with the feed, and
// unreadTotal no longer wraps its count in DISTINCT. A message filed twice must
// still be counted once, and the count must agree with the id list.
func TestUnreadTotalCountsFiledTwiceOnce(t *testing.T) {
	store := openTestStore(t)
	fixture := seedScopeFixture(t, store)
	ctx := context.Background()
	for _, filter := range []ports.MessageFilter{
		{},
		{AccountID: &fixture.account.ID},
		{Folder: "inbox"},
		{MailboxID: &fixture.archive.ID},
		{AccountID: &fixture.account.ID, Folder: "inbox", Query: "hello"},
	} {
		total, err := store.unreadTotal(ctx, filter)
		if err != nil {
			t.Fatalf("unread total for %+v: %v", filter, err)
		}
		ids, err := store.UnreadMessageIDs(ctx, filter, 0)
		if err != nil {
			t.Fatalf("unread ids for %+v: %v", filter, err)
		}
		if total != len(ids) {
			t.Fatalf("filter %+v: unread total = %d but id list has %d entries (%v)", filter, total, len(ids), ids)
		}
		seen := map[int64]int{}
		for _, id := range ids {
			seen[id]++
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("filter %+v listed unread message %d %d times", filter, id, n)
			}
		}
	}
}

// Keyset pagination has to keep working without DISTINCT: the cursor is applied
// to the same query, and a page boundary must not drop or repeat a row.
func TestFeedPaginationCoversEveryRowOnce(t *testing.T) {
	store := openTestStore(t)
	fixture := seedScopeFixture(t, store)
	ctx := context.Background()
	want := feedIDs(t, store, ports.MessageFilter{Folder: "inbox"})

	var got []int64
	cursor := ""
	for range 10 {
		page, err := store.ListMessages(ctx, ports.MessageFilter{Folder: "inbox", Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			got = append(got, item.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if !equalIDs(got, want) {
		t.Fatalf("paged through %v, want %v (account %d)", got, want, fixture.account.ID)
	}
}
