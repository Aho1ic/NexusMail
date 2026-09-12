//go:build sqlite_fts5

package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"nexusmail/internal/domain"
)

func seedDraft(t *testing.T, store *Store, accountID int64, status string, attempt int, nextAttemptAt *int64) domain.Draft {
	t.Helper()
	now := time.Now().UnixMilli()
	draft := domain.Draft{
		AccountID: accountID, RFCMessageID: "<" + itoa(int(time.Now().UnixNano()%1_000_000_000)) + "@nexusmail.local>", Revision: 1,
		ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", Status: status, RemoteSyncState: "dirty",
		AttemptCount: attempt, NextAttemptAt: nextAttemptAt,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateDraft(context.Background(), &draft); err != nil {
		t.Fatal(err)
	}
	return draft
}

// TestClaimSendableDraftHonoursBackoff is the path worker.Queue takes on a
// double-click of retry: a draft already sitting in retry_wait with a future
// next_attempt_at must not be claimed, or the 5s/30s/2m/10m ladder is skipped
// and the attempt budget is spent on back-to-back sends.
func TestClaimSendableDraftHonoursBackoff(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, _ := seedAccountMailbox(t, store)

	future := time.Now().Add(5 * time.Second).UnixMilli()
	waiting := seedDraft(t, store, account.ID, "retry_wait", 1, &future)

	if _, _, err := store.ClaimSendableDraft(ctx, waiting.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("claim of a retry_wait draft whose deadline is in the future: err=%v, want ErrConflict", err)
	}
	stored, _, err := store.GetDraft(ctx, waiting.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "retry_wait" || stored.AttemptCount != 1 {
		t.Fatalf("backoff-skipped claim mutated the draft: status=%q attempt=%d", stored.Status, stored.AttemptCount)
	}

	// Once the deadline has elapsed the same draft is claimable.
	past := time.Now().Add(-time.Millisecond).UnixMilli()
	if err := store.SetDraftDelivery(ctx, waiting.ID, "retry_wait", 1, &past, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := store.ClaimSendableDraft(ctx, waiting.ID)
	if err != nil {
		t.Fatalf("claim of a due retry_wait draft: %v", err)
	}
	if claimed.Status != "sending" || claimed.AttemptCount != 2 {
		t.Fatalf("due claim left status=%q attempt=%d, want sending/2", claimed.Status, claimed.AttemptCount)
	}
}

// A queued draft is "send now" even if it still carries a leftover next_attempt_at
// from an earlier retry round. The backoff check applies only to retry_wait.
func TestClaimSendableDraftDoesNotHoldQueued(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, _ := seedAccountMailbox(t, store)

	future := time.Now().Add(time.Hour).UnixMilli()
	queued := seedDraft(t, store, account.ID, "queued", 0, &future)
	claimed, _, err := store.ClaimSendableDraft(ctx, queued.ID)
	if err != nil {
		t.Fatalf("queued draft with a leftover next_attempt_at was refused: %v", err)
	}
	if claimed.Status != "sending" || claimed.AttemptCount != 1 {
		t.Fatalf("queued claim left status=%q attempt=%d, want sending/1", claimed.Status, claimed.AttemptCount)
	}

	// A queued draft with no deadline at all is the ordinary first-send path.
	plain := seedDraft(t, store, account.ID, "queued", 0, nil)
	if _, _, err := store.ClaimSendableDraft(ctx, plain.ID); err != nil {
		t.Fatalf("queued draft with no deadline was refused: %v", err)
	}
}

// A retry_wait draft whose next_attempt_at is NULL is eligible: the deadline is
// unknown, which is not the same as "wait". The IS NULL half of the predicate
// is what lets a row written without one still be claimed.
func TestClaimSendableDraftAcceptsRetryWaitWithNoDeadline(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, _ := seedAccountMailbox(t, store)
	waiting := seedDraft(t, store, account.ID, "retry_wait", 2, nil)
	claimed, _, err := store.ClaimSendableDraft(ctx, waiting.ID)
	if err != nil {
		t.Fatalf("retry_wait with no next_attempt_at: %v", err)
	}
	if claimed.Status != "sending" || claimed.AttemptCount != 3 {
		t.Fatalf("status=%q attempt=%d, want sending/3", claimed.Status, claimed.AttemptCount)
	}
}

func TestClaimSendableDraftRejectsUnsendableStatuses(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, _ := seedAccountMailbox(t, store)
	for _, status := range []string{"draft", "sending", "failed", "unknown", "sent"} {
		d := seedDraft(t, store, account.ID, status, 0, nil)
		if _, _, err := store.ClaimSendableDraft(ctx, d.ID); !errors.Is(err, ErrConflict) {
			t.Errorf("status %q: err=%v, want ErrConflict", status, err)
		}
	}
}

// RecoverSendingDrafts used to leave attempt_count where it was. A draft that
// crashed on attempt 5 then went to 6 on the next claim, failed the
// AttemptCount < 5 guard, and became 'failed' without a retry — unlike the
// SMTP-level 'unknown' path, which already reset the counter to 0 so a manual
// retry is a clean restart.
func TestRecoverSendingDraftsResetsAttemptCount(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, _ := seedAccountMailbox(t, store)
	sending := seedDraft(t, store, account.ID, "sending", 5, nil)
	queued := seedDraft(t, store, account.ID, "queued", 2, nil)

	if err := store.RecoverSendingDrafts(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := store.GetDraft(ctx, sending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != "unknown" {
		t.Fatalf("status = %q, want unknown", recovered.Status)
	}
	if recovered.AttemptCount != 0 {
		t.Fatalf("attempt_count = %d after recovery, want 0 so a later claim starts a new budget", recovered.AttemptCount)
	}
	if recovered.LastError == nil || *recovered.LastError == "" {
		t.Fatal("recovery left last_error empty")
	}

	// Other statuses are not the recovery's concern.
	untouched, _, err := store.GetDraft(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if untouched.Status != "queued" || untouched.AttemptCount != 2 {
		t.Fatalf("queued draft mutated: status=%q attempt=%d", untouched.Status, untouched.AttemptCount)
	}

	// The recovered draft can now be requeued and claimed without immediately
	// exhausting the budget the next fail() consults.
	if err := store.SetDraftDelivery(ctx, sending.ID, "queued", recovered.AttemptCount, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := store.ClaimSendableDraft(ctx, sending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.AttemptCount != 1 {
		t.Fatalf("first claim after recovery: attempt_count = %d, want 1", claimed.AttemptCount)
	}
}
