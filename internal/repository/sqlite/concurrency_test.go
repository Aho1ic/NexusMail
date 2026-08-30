//go:build sqlite_fts5

package sqlite

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

// The store serialises every write behind one writeMu while reads run unlocked
// against an 8-connection pool. The tests below drive both at once: under WAL a
// write that escaped the mutex shows up as SQLITE_BUSY, and a read that took the
// mutex would show up as the whole suite serialising to a crawl rather than as a
// failure, so the ones that matter assert on errors rather than on timing.

func concurrentFixture(t *testing.T) (*Store, domain.Account, domain.Mailbox) {
	t.Helper()
	store := openTestStore(t)
	account, mailbox := seedAccountMailbox(t, store)
	return store, account, mailbox
}

func ingestBatch(store *Store, accountID, mailboxID int64, firstUID uint32, count int) error {
	items := make([]ports.MessageInput, 0, count)
	for i := 0; i < count; i++ {
		uid := firstUID + uint32(i)
		digest := sha256.Sum256([]byte(fmt.Sprintf("concurrent:%d:%d", mailboxID, uid)))
		now := time.Now().UnixMilli()
		message := &domain.Message{
			AccountID: accountID, Direction: "incoming", DedupeKey: digest[:],
			Subject: fmt.Sprintf("Concurrent %d", uid), Sender: "sender@example.com", Recipients: "me@example.com",
			FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
			Snippet: "snippet", BodyState: "ready",
			ReceivedAt: now, CreatedAt: now, UpdatedAt: now,
		}
		items = append(items, ports.MessageInput{Message: message, MailboxID: mailboxID, UID: uid, InternalDate: time.Now()})
	}
	_, _, err := store.BatchCreateOrUpdateMessages(context.Background(), items)
	return err
}

// Every write path holds writeMu. Driving several of them at once from many
// goroutines must produce no SQLITE_BUSY and no lost row: WAL allows one writer,
// so a write method that forgot the mutex fails here rather than in production
// under a burst from the 5-second probe.
func TestConcurrentWritesNeverContend(t *testing.T) {
	store, account, mailbox := concurrentFixture(t)
	ctx := context.Background()

	const workers, perWorker = 8, 12
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker*4)

	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			for round := 0; round < perWorker; round++ {
				// Message ingest, the hottest write path.
				if err := ingestBatch(store, account.ID, mailbox.ID, uint32(index*1000+round*10+1), 5); err != nil {
					errs <- fmt.Errorf("ingest: %w", err)
				}
				// A mailbox cursor update, which the sync loop does after every pass.
				if err := store.UpdateMailboxCursor(ctx, mailbox.ID, mailbox.UIDValidity, uint32(index*1000+round), nil, nil); err != nil {
					errs <- fmt.Errorf("cursor: %w", err)
				}
				// A draft write, which the UI does on every autosave.
				draft := domain.Draft{
					AccountID: account.ID, RFCMessageID: fmt.Sprintf("<%d-%d@nexusmail.local>", index, round),
					Revision: 1, ToJSON: `[]`, CCJSON: `[]`, BCCJSON: `[]`, Status: "draft",
					RemoteSyncState: "dirty", CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
				}
				if err := store.CreateDraft(ctx, &draft); err != nil {
					errs <- fmt.Errorf("draft: %w", err)
				}
				// A blob insert, which the body prefetcher does per attachment.
				sum := sha256.Sum256([]byte(fmt.Sprintf("blob:%d:%d", index, round)))
				blob := domain.BlobObject{StorageKey: fmt.Sprintf("%02x/%x", sum[0], sum[1:8]), SHA256: sum[:], SizeBytes: 16, Durability: "cache", LastAccessedAt: time.Now().UnixMilli(), CreatedAt: time.Now().UnixMilli()}
				if err := store.CreateBlob(ctx, &blob); err != nil {
					errs <- fmt.Errorf("blob: %w", err)
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write failed: %v", err)
	}

	// Every message landed: workers*perWorker*5 distinct dedupe keys.
	page, err := store.ListMessages(ctx, ports.MessageFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if page.UnreadTotal != workers*perWorker*5 {
		t.Fatalf("stored %d messages, want %d", page.UnreadTotal, workers*perWorker*5)
	}
	drafts, err := store.ListDrafts(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != workers*perWorker {
		t.Fatalf("stored %d drafts, want %d", len(drafts), workers*perWorker)
	}
}

// Reads run without the mutex, so they must stay available while writes are in
// flight and must never see a partial batch: ingest commits N rows in one
// transaction, so a reader sees either none of a batch or all of it.
func TestReadsStayConsistentDuringWrites(t *testing.T) {
	store, account, mailbox := concurrentFixture(t)
	ctx := context.Background()

	const batches, batchSize = 40, 5
	stop := make(chan struct{})
	writeErr := make(chan error, 1)
	go func() {
		defer close(stop)
		for i := 0; i < batches; i++ {
			if err := ingestBatch(store, account.ID, mailbox.ID, uint32(i*batchSize+1), batchSize); err != nil {
				select {
				case writeErr <- err:
				default:
				}
				return
			}
		}
	}()

	var reads atomic.Int64
	var readers sync.WaitGroup
	readErrs := make(chan error, 64)
	for i := 0; i < 6; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				page, err := store.ListMessages(ctx, ports.MessageFilter{Limit: 100})
				if err != nil {
					readErrs <- err
					return
				}
				// Ingest commits a whole batch at once, so a reader can only ever see
				// a multiple of the batch size.
				if page.UnreadTotal%batchSize != 0 {
					readErrs <- fmt.Errorf("read saw %d messages, not a whole number of batches", page.UnreadTotal)
					return
				}
				reads.Add(1)
			}
		}()
	}
	readers.Wait()
	close(readErrs)
	select {
	case err := <-writeErr:
		t.Fatalf("write failed: %v", err)
	default:
	}
	for err := range readErrs {
		t.Fatalf("read failed: %v", err)
	}
	if reads.Load() == 0 {
		t.Fatal("no reads completed while writes were running")
	}
}

// The same batch ingested from several goroutines at once must dedupe to one row
// per key. This is the guarantee that makes a mailbox re-sync idempotent, and it is
// where the blobArg fix matters: a bare []byte dedupe key is expanded into 32
// integer comparisons, so the lookup misses and the unique index rolls the batch
// back.
func TestConcurrentIngestOfTheSameBatchDedupes(t *testing.T) {
	store, account, mailbox := concurrentFixture(t)
	ctx := context.Background()

	const workers = 10
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := ingestBatch(store, account.ID, mailbox.ID, 1, 20); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent ingest of the same batch failed: %v", err)
	}

	page, err := store.ListMessages(ctx, ports.MessageFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if page.UnreadTotal != 20 {
		t.Fatalf("%d workers ingesting the same 20 messages produced %d rows", workers, page.UnreadTotal)
	}
}

// The optimistic lock on drafts must admit exactly one of N writers racing on the
// same revision, and the winner's value must be what is stored.
func TestConcurrentDraftUpdatesElectOneWinner(t *testing.T) {
	store, account, _ := concurrentFixture(t)
	ctx := context.Background()
	draft := domain.Draft{
		AccountID: account.ID, RFCMessageID: "<race@nexusmail.local>", Revision: 1,
		ToJSON: `[]`, CCJSON: `[]`, BCCJSON: `[]`, Subject: "original", Status: "draft",
		RemoteSyncState: "dirty", CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	if err := store.CreateDraft(ctx, &draft); err != nil {
		t.Fatal(err)
	}

	const writers = 16
	var wg sync.WaitGroup
	var winners atomic.Int64
	var conflicts atomic.Int64
	unexpected := make(chan error, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			candidate := draft
			candidate.Subject = fmt.Sprintf("writer-%d", index)
			candidate.UpdatedAt = time.Now().UnixMilli()
			<-start
			switch err := store.UpdateDraft(ctx, &candidate, 1); {
			case err == nil:
				winners.Add(1)
			case errors.Is(err, ports.ErrConflict):
				conflicts.Add(1)
			default:
				unexpected <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(unexpected)
	for err := range unexpected {
		t.Fatalf("unexpected error: %v", err)
	}
	if winners.Load() != 1 || conflicts.Load() != writers-1 {
		t.Fatalf("winners = %d, conflicts = %d, want 1 and %d", winners.Load(), conflicts.Load(), writers-1)
	}
	stored, _, err := store.GetDraft(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != 2 {
		t.Fatalf("revision = %d after %d racing writers, want 2", stored.Revision, writers)
	}
	if stored.Subject == "original" {
		t.Fatal("the winner's value was not stored")
	}
}

// Bulk flag updates from several goroutines must all land. UpdateMessages writes a
// chunked IN list under the mutex; a chunk that escaped it would be lost.
func TestConcurrentBulkFlagUpdates(t *testing.T) {
	store, account, mailbox := concurrentFixture(t)
	ctx := context.Background()
	if err := ingestBatch(store, account.ID, mailbox.ID, 1, 120); err != nil {
		t.Fatal(err)
	}
	ids, err := store.UnreadMessageIDs(ctx, ports.MessageFilter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 120 {
		t.Fatalf("unread ids = %d, want 120", len(ids))
	}

	// Six goroutines each mark a disjoint slice read.
	const workers = 6
	slice := len(ids) / workers
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	value := true
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			chunk := ids[index*slice : (index+1)*slice]
			if err := store.UpdateMessages(ctx, chunk, ports.MessagePatch{IsRead: &value}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("bulk update failed: %v", err)
	}

	remaining, err := store.UnreadMessageIDs(ctx, ports.MessageFilter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("%d messages are still unread after every slice was marked read", len(remaining))
	}
}

// The unread count is memoised with a short TTL. Concurrent readers and writers
// must not be able to leave a stale value visible for longer than that, and the
// cache must never hand out a count from a different view.
func TestUnreadCacheStaysCorrectUnderConcurrency(t *testing.T) {
	store, account, mailbox := concurrentFixture(t)
	other, otherMailbox := seedSecondAccount(t, store)
	ctx := context.Background()
	if err := ingestBatch(store, account.ID, mailbox.ID, 1, 10); err != nil {
		t.Fatal(err)
	}
	if err := ingestBatch(store, other.ID, otherMailbox.ID, 1, 3); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 20; round++ {
				first, err := store.ListMessages(ctx, ports.MessageFilter{AccountID: &account.ID, Limit: 50})
				if err != nil {
					errs <- err
					return
				}
				second, err := store.ListMessages(ctx, ports.MessageFilter{AccountID: &other.ID, Limit: 50})
				if err != nil {
					errs <- err
					return
				}
				// Two different views must never share a cached count.
				if first.UnreadTotal == second.UnreadTotal {
					errs <- fmt.Errorf("both accounts reported %d unread", first.UnreadTotal)
					return
				}
				if first.UnreadTotal != 10 || second.UnreadTotal != 3 {
					errs <- fmt.Errorf("unread totals = %d and %d, want 10 and 3", first.UnreadTotal, second.UnreadTotal)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("%v", err)
	}
}

func seedSecondAccount(t *testing.T, store *Store) (domain.Account, domain.Mailbox) {
	t.Helper()
	now := time.Now().UnixMilli()
	account := domain.Account{
		Email: "second@example.com", DisplayName: "Second", Provider: "163", AuthType: "password",
		Username: "second@example.com", IMAPHost: "imap.163.com", IMAPPort: 993, IMAPTLSMode: "implicit",
		SMTPHost: "smtp.163.com", SMTPPort: 465, SMTPTLSMode: "implicit",
		SecretCiphertext: []byte("sealed"), Status: "connected", CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateAccount(context.Background(), &account); err != nil {
		t.Fatal(err)
	}
	mailbox := domain.Mailbox{AccountID: account.ID, RemoteName: "INBOX", DisplayName: "INBOX", Role: "inbox", SyncMode: "realtime", UIDValidity: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertMailbox(context.Background(), &mailbox); err != nil {
		t.Fatal(err)
	}
	boxes, err := store.ListMailboxes(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	return account, boxes[0]
}

// A closed store must fail every subsequent call cleanly rather than panicking:
// shutdown races with in-flight work by construction.
func TestClosedStoreFailsCleanly(t *testing.T) {
	store, account, mailbox := concurrentFixture(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Ping(ctx); err == nil {
		t.Fatal("Ping succeeded on a closed store")
	}
	if _, err := store.ListMessages(ctx, ports.MessageFilter{}); err == nil {
		t.Fatal("ListMessages succeeded on a closed store")
	}
	if err := ingestBatch(store, account.ID, mailbox.ID, 1, 2); err == nil {
		t.Fatal("ingest succeeded on a closed store")
	}
}
