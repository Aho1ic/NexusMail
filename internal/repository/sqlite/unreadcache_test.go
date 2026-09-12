//go:build sqlite_fts5

package sqlite

import (
	"context"
	"testing"
	"time"

	"nexusmail/internal/ports"
)

// The filter carries *int64 and *bool, and the transport allocates a fresh
// pointer for every request (optionalInt64 in transport/http/errors.go). A cache
// keyed on those pointers compares addresses, so no two requests for the same
// view ever agreed: every scoped feed load re-ran the count and stored another
// entry that nothing would ever read.
//
// Each round below allocates its own pointers, which is what the production
// caller does and what the existing concurrency test does not — that one reuses
// one &account.ID across all of its rounds and passes either way.
func TestUnreadCacheHitsAcrossSeparatelyAllocatedFilters(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()
	seedMessage(t, store, account.ID, mailbox.ID, 1, "one", "incoming", false, now)
	seedMessage(t, store, account.ID, mailbox.ID, 2, "two", "incoming", false, now+1)

	// Fresh pointers per call, exactly as a second HTTP request would build them.
	scoped := func() ports.MessageFilter {
		accountID := account.ID
		mailboxID := mailbox.ID
		isRead := false
		return ports.MessageFilter{AccountID: &accountID, MailboxID: &mailboxID, Folder: "inbox", IsRead: &isRead}
	}

	count, hit, err := store.cachedUnreadTotal(ctx, scoped())
	if err != nil {
		t.Fatalf("first count: %v", err)
	}
	if hit {
		t.Fatal("first call reported a cache hit on an empty cache")
	}
	if count != 2 {
		t.Fatalf("unread = %d, want 2", count)
	}

	second, hit, err := store.cachedUnreadTotal(ctx, scoped())
	if err != nil {
		t.Fatalf("second count: %v", err)
	}
	if !hit {
		t.Fatal("a repeat of the same scoped view missed the cache; the key is comparing pointer addresses, not values")
	}
	if second != count {
		t.Fatalf("cached unread = %d, want %d", second, count)
	}

	// One entry per view, not one per request: the miss path used to store a
	// second unreachable entry on every call.
	store.unreadMu.Lock()
	entries := len(store.unreadCache)
	store.unreadMu.Unlock()
	if entries != 1 {
		t.Fatalf("cache holds %d entries for one view, want 1", entries)
	}
}

// Value semantics must not go so far as to collapse "not specified" into the
// zero value: account_id=0 selects nothing, while an absent account_id selects
// every account. A key that dropped the has* flags would serve one view's count
// for the other.
func TestUnreadCacheSeparatesUnsetFromZero(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()
	seedMessage(t, store, account.ID, mailbox.ID, 1, "one", "incoming", false, now)
	seedMessage(t, store, account.ID, mailbox.ID, 2, "two", "incoming", false, now+1)

	unscoped, _, err := store.cachedUnreadTotal(ctx, ports.MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if unscoped != 2 {
		t.Fatalf("unscoped unread = %d, want 2", unscoped)
	}

	// No account has id 0 — the column is AUTOINCREMENT from 1 — so this view is
	// empty, and a cached 2 here would be the unscoped count leaking across.
	zero := int64(0)
	zeroAccount, hit, err := store.cachedUnreadTotal(ctx, ports.MessageFilter{AccountID: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("account_id=0 was answered from the entry stored for the unscoped view")
	}
	if zeroAccount != 0 {
		t.Fatalf("account_id=0 unread = %d, want 0", zeroAccount)
	}

	zeroMailbox, hit, err := store.cachedUnreadTotal(ctx, ports.MessageFilter{MailboxID: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("mailbox_id=0 was answered from another view's entry")
	}
	if zeroMailbox != 0 {
		t.Fatalf("mailbox_id=0 unread = %d, want 0", zeroMailbox)
	}

	// is_read is part of the key too. It does not change unreadTotal today, but a
	// pointer-to-false and an absent value must not be the same key, or a future
	// unreadTotal that reads the filter would inherit a silently wrong entry.
	isRead := false
	if _, hit, err := store.cachedUnreadTotal(ctx, ports.MessageFilter{IsRead: &isRead}); err != nil || hit {
		t.Fatalf("is_read=false hit=%v err=%v, want a miss distinct from the unscoped entry", hit, err)
	}
}

// Folder and Query arrive from the query string, so the key space is not limited
// to views the UI can produce. Entries are only dropped by a write, which on an
// idle account may never come, so the table needs a bound of its own.
func TestUnreadCacheIsBounded(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	seedMessage(t, store, account.ID, mailbox.ID, 1, "one", "incoming", false, time.Now().UnixMilli())

	for i := range unreadCacheEntries * 3 {
		// Distinct search terms, the shape a client typing into the search box
		// produces one keystroke at a time.
		if _, _, err := store.cachedUnreadTotal(ctx, ports.MessageFilter{Query: "term" + itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	store.unreadMu.Lock()
	entries := len(store.unreadCache)
	store.unreadMu.Unlock()
	if entries > unreadCacheEntries {
		t.Fatalf("cache holds %d entries after %d distinct views, want at most %d", entries, unreadCacheEntries*3, unreadCacheEntries)
	}
}

// The cache may not outlive the write that changes what it counted.
func TestUnreadCacheInvalidatedByMarkRead(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	now := time.Now().UnixMilli()
	first := seedMessage(t, store, account.ID, mailbox.ID, 1, "one", "incoming", false, now)
	seedMessage(t, store, account.ID, mailbox.ID, 2, "two", "incoming", false, now+1)

	filter := func() ports.MessageFilter {
		accountID := account.ID
		return ports.MessageFilter{AccountID: &accountID}
	}
	if count, _, err := store.cachedUnreadTotal(ctx, filter()); err != nil || count != 2 {
		t.Fatalf("initial unread = %d err=%v, want 2", count, err)
	}
	value := true
	if _, err := store.UpdateMessage(ctx, first, ports.MessagePatch{IsRead: &value}); err != nil {
		t.Fatal(err)
	}
	count, hit, err := store.cachedUnreadTotal(ctx, filter())
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("mark-read left a usable cache entry behind")
	}
	if count != 1 {
		t.Fatalf("unread after mark-read = %d, want 1", count)
	}
}
