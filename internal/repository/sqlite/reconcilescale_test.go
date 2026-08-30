//go:build sqlite_fts5

package sqlite

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

// TestUpsertMailboxKeepsSyncCursor covers a cursor being destroyed by the pass
// that is only supposed to record names and roles.
//
// Every full sync re-upserts each mailbox from the LIST response, where uid_next
// and highest_modseq are always nil. The conflict clause copied those nils over
// the cursor, so the cursor written by the previous sync was wiped seconds later.
// Nothing failed visibly: the app just lost the two values that let it decide a
// mailbox needs no work, and re-walked every mailbox on every tick instead.
func TestUpsertMailboxKeepsSyncCursor(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)

	uidNext := uint32(4211)
	modSeq := uint64(90210)
	if err := store.UpdateMailboxCursor(ctx, mailbox.ID, 42, 4210, &uidNext, &modSeq); err != nil {
		t.Fatal(err)
	}

	// The same mailbox as a later LIST reports it: named and classified, no cursor.
	now := time.Now().UnixMilli()
	relisted := domain.Mailbox{AccountID: account.ID, RemoteName: mailbox.RemoteName, DisplayName: "Inbox",
		Role: "inbox", SyncMode: "realtime", UIDValidity: 42, CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertMailbox(ctx, &relisted); err != nil {
		t.Fatal(err)
	}

	after, err := store.GetMailbox(ctx, mailbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.UIDNext == nil || *after.UIDNext != uidNext {
		t.Errorf("uid_next = %v after re-list, want %d", after.UIDNext, uidNext)
	}
	if after.HighestModSeq == nil || *after.HighestModSeq != modSeq {
		t.Errorf("highest_modseq = %v after re-list, want %d", after.HighestModSeq, modSeq)
	}
	if after.LastUID != 4210 {
		t.Errorf("last_uid = %d after re-list, want 4210", after.LastUID)
	}
}

// TestReconcileScaleCost measures what one reconciliation pass costs on a
// mailbox the size of a real Gmail or QQ account. Both calls take writeMu, which
// serialises every other write in the process — including the insert of a newly
// arrived message — so their duration is added directly to new-mail latency.
func TestReconcileScaleCost(t *testing.T) {
	const total = 20000
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)

	seed := time.Now()
	bulkSeed(t, store, account.ID, mailbox.ID, total)
	t.Logf("seeded %d messages in %s", total, time.Since(seed))

	states := make([]ports.RemoteFlagState, total)
	for index := range total {
		states[index] = ports.RemoteFlagState{UID: uint32(index + 1), Flags: []string{}}
	}

	start := time.Now()
	uids, err := store.ListMailboxUIDs(ctx, mailbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(uids) != total {
		t.Fatalf("ListMailboxUIDs returned %d, want %d", len(uids), total)
	}
	t.Logf("ListMailboxUIDs (%d rows) took %s", total, time.Since(start))

	start = time.Now()
	changed, err := store.ReconcileMailboxFlags(ctx, mailbox.ID, states)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	t.Logf("ReconcileMailboxFlags (%d states, none differ) took %s, changed=%d", total, elapsed, changed)
	// Generous next to the ~80ms this measures, but it fails loudly if the pass ever
	// grows a per-row query or drops the chunking: writeMu is held throughout, so
	// this duration is time a newly arrived message cannot be inserted.
	if elapsed > 3*time.Second {
		t.Errorf("reconciling %d unchanged rows held writeMu for %s", total, elapsed)
	}

	start = time.Now()
	changed, err = store.ReconcileMailboxFlags(ctx, mailbox.ID, states)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ReconcileMailboxFlags identical second pass took %s, changed=%d", time.Since(start), changed)

	// One stale UID is enough to trigger the orphan sweep, which is the expensive
	// part of the delete path rather than the chunked DELETE itself.
	start = time.Now()
	removed, err := store.DeleteMailboxUIDs(ctx, mailbox.ID, []uint32{uint32(total)})
	if err != nil {
		t.Fatal(err)
	}
	elapsed = time.Since(start)
	t.Logf("DeleteMailboxUIDs with 1 stale UID took %s, removed=%d", elapsed, removed)
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	// writeMu is held for the whole call, so this is time no newly arrived message
	// can be inserted. The sweep used to be `id NOT IN (SELECT message_id FROM
	// mailbox_messages)` across the whole table, which cost the same whether one
	// UID or ten thousand were expunged.
	if elapsed > time.Second {
		t.Errorf("expunging 1 uid out of %d held writeMu for %s", total, elapsed)
	}
}

// TestOrphanSweepIsIndexed asserts the plan rather than the clock: a full scan of
// messages is fast enough at test scale to pass a timing bound while still costing
// seconds on a real 100k-message account.
func TestOrphanSweepIsIndexed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	bulkSeed(t, store, account.ID, mailbox.ID, 200)

	var steps []struct {
		Detail string `gorm:"column:detail"`
	}
	// The statement under test is the shipped constant, so reverting the sweep to an
	// unbounded form fails here rather than only showing up as latency in production.
	err := store.db.WithContext(ctx).Raw("EXPLAIN QUERY PLAN "+orphanSweepSQL, []int64{1, 2, 3}).Scan(&steps).Error
	if err != nil {
		t.Fatal(err)
	}
	plan := make([]string, 0, len(steps))
	for _, step := range steps {
		plan = append(plan, step.Detail)
	}
	t.Logf("orphan sweep plan: %v", plan)
	for _, detail := range plan {
		// "SCAN messages" is the shape being ruled out; "SEARCH messages" over the
		// rowid list is the shape being required.
		if strings.Contains(detail, "SCAN messages") {
			t.Fatalf("orphan sweep scans the whole messages table: %v", plan)
		}
	}
	if !slices.ContainsFunc(plan, func(detail string) bool { return strings.Contains(detail, "SEARCH messages") }) {
		t.Fatalf("orphan sweep does not look up messages by rowid: %v", plan)
	}
}

// bulkSeed inserts count messages through the production write path so the FTS
// triggers and indexes are exercised the same way a real sync exercises them.
func bulkSeed(t *testing.T, store *Store, accountID, mailboxID int64, count int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	for index := range count {
		subject := fmt.Sprintf("scale %d", index)
		digest := sha256.Sum256([]byte(subject))
		message := domain.Message{
			AccountID: accountID, Direction: "incoming", DedupeKey: digest[:], Subject: subject,
			Sender: "Sender <sender@example.com>", Recipients: "receiver@example.com",
			FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
			BodyState: "ready", SizeBytes: 2048, ReceivedAt: now + int64(index), CreatedAt: now, UpdatedAt: now,
		}
		created, err := ingestOne(ctx, store, &message, mailboxID, uint32(index)+1, nil, time.UnixMilli(now))
		if err != nil || !created {
			t.Fatalf("seed %d: created=%v err=%v", index, created, err)
		}
	}
}
