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
	changed := 0
	counter := countSQL(t, store, func() {
		changed, err = store.ReconcileMailboxFlags(ctx, mailbox.ID, states)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ReconcileMailboxFlags (%d states) took %s, changed=%d, sql: %s",
		total, time.Since(start), changed, counter.summary())
	// writeMu is held for the whole pass, so its cost lands directly on the latency
	// of a message arriving at the same moment. What keeps it affordable is that it
	// reads in chunks, and that is a statement count, not a duration: the same pass
	// measures 4.9s under the race detector and 97ms without, so no wall-clock bound
	// is both stable and tight enough to notice the chunking going away. Reading
	// per row would issue one SELECT per state instead of one per chunk.
	chunks := (total + sqliteParameterChunk - 1) / sqliteParameterChunk
	if selects := counter.count("SELECT"); selects > chunks {
		t.Errorf("reconciling %d rows issued %d SELECTs, want at most %d (one per %d-row chunk)",
			total, selects, chunks, sqliteParameterChunk)
	}

	// The second pass is the one every 5-minute tick actually runs: the provider
	// reports what the local rows already say. It has to be free of writes. Only
	// this pass can carry that assertion — the first one legitimately rewrites all
	// 20000 flags_json values, because bulkSeed stores nil flags as `null` and these
	// states marshal an empty slice to `[]`.
	start = time.Now()
	counter = countSQL(t, store, func() {
		changed, err = store.ReconcileMailboxFlags(ctx, mailbox.ID, states)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ReconcileMailboxFlags identical second pass took %s, changed=%d, sql: %s",
		time.Since(start), changed, counter.summary())
	if writes := counter.count("UPDATE"); writes != 0 {
		t.Errorf("reconciling %d unchanged rows issued %d UPDATEs, want 0", total, writes)
	}
	if selects := counter.count("SELECT"); selects > chunks {
		t.Errorf("second pass issued %d SELECTs, want at most %d", selects, chunks)
	}
	if changed != 0 {
		t.Errorf("changed = %d on a pass where no flag differs, want 0", changed)
	}

	// One stale UID is enough to trigger the orphan sweep, which is the expensive
	// part of the delete path rather than the chunked DELETE itself.
	start = time.Now()
	removed, err := store.DeleteMailboxUIDs(ctx, mailbox.ID, []uint32{uint32(total)})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("DeleteMailboxUIDs with 1 stale UID took %s, removed=%d", time.Since(start), removed)
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	// The cost that matters here is the sweep's shape, not this duration: the sweep
	// used to be `id NOT IN (SELECT message_id FROM mailbox_messages)` across the
	// whole table, which costs the same whether one UID or ten thousand are
	// expunged. TestOrphanSweepIsIndexed asserts that shape against the query plan,
	// which a timing bound at test scale cannot do.
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
