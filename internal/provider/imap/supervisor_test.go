//go:build sqlite_fts5

package imap

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"

	goimap "github.com/emersion/go-imap/v2"
)

// TestIsAuthFailure separates a wrong password from a broken network. The two need
// opposite handling: a network fault should keep retrying on the 1s→5m ladder, while
// bad credentials should back off to 15 minutes and surface as auth_error, because
// retrying every second only gets the account locked out by the provider.
func TestIsAuthFailure(t *testing.T) {
	authFailures := []error{
		&goimap.Error{Code: goimap.ResponseCodeAuthenticationFailed, Text: "no"},
		&goimap.Error{Code: goimap.ResponseCodeAuthorizationFailed, Text: "no"},
		&goimap.Error{Code: goimap.ResponseCodeExpired, Text: "credentials expired"},
		&goimap.Error{Code: goimap.ResponseCodePrivacyRequired, Text: "tls required"},
		errors.New("imap: AUTHENTICATIONFAILED bad user or password"),
		errors.New("Invalid credentials (Failure)"),
		errors.New("LOGIN denied"),
		errors.New("refresh oauth token: invalid_grant"),
		fmt.Errorf("connect: %w", errors.New("authentication failed")),
	}
	for _, err := range authFailures {
		if !isAuthFailure(err) {
			t.Errorf("isAuthFailure(%v) = false, want true", err)
		}
	}

	transient := []error{
		nil,
		errors.New("dial tcp 1.2.3.4:993: i/o timeout"),
		errors.New("connection reset by peer"),
		errors.New("imap: SERVERBUG internal error"),
		errors.New("EOF"),
		&goimap.Error{Code: goimap.ResponseCodeUnavailable, Text: "try later"},
		&goimap.Error{Code: goimap.ResponseCodeInUse, Text: "mailbox busy"},
		context.Canceled,
	}
	for _, err := range transient {
		if isAuthFailure(err) {
			t.Errorf("isAuthFailure(%v) = true, want false", err)
		}
	}
}

// TestRecordBodyAttemptCountsOnlyRealFailures underpins the prefetch cap. The
// candidate query cannot exclude the error state without a schema change, so a body
// that cannot be fetched was re-queued on every 5-second probe forever, permanently
// occupying the workers that new mail needs.
func TestRecordBodyAttemptCountsOnlyRealFailures(t *testing.T) {
	h := newHarness(t)
	supervisor := h.supervisor
	const id int64 = 7

	failure := errors.New("fetch body: unexpected EOF")
	for round := 1; round <= maxBodyAttempts; round++ {
		supervisor.recordBodyAttempt(id, failure)
		if got := attempts(t, supervisor, id); got != round {
			t.Fatalf("after %d failures the count is %d", round, got)
		}
	}

	// Cancellation is the process shutting down, not the message misbehaving.
	supervisor.recordBodyAttempt(id, context.Canceled)
	if got := attempts(t, supervisor, id); got != maxBodyAttempts {
		t.Fatalf("cancellation counted as a failure: %d", got)
	}
	supervisor.recordBodyAttempt(id, fmt.Errorf("fetch: %w", context.Canceled))
	if got := attempts(t, supervisor, id); got != maxBodyAttempts {
		t.Fatalf("wrapped cancellation counted as a failure: %d", got)
	}

	// A success clears the record, so a message that recovers is not held against
	// its earlier failures.
	supervisor.recordBodyAttempt(id, nil)
	if _, ok := supervisor.bodyAttempts.Load(id); ok {
		t.Fatal("success did not clear the attempt count")
	}
}

// TestEnqueueBodyCandidatesStopsAfterCap is the behavioural half of the same fix:
// once the cap is reached the id must not be queued again. The supervisor is not
// started, so nothing else touches the queue while the assertion runs.
func TestEnqueueBodyCandidatesStopsAfterCap(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	supervisor := h.supervisor
	id := seedBodyCandidate(t, h)

	supervisor.bodyAttempts.Store(id, maxBodyAttempts)
	supervisor.enqueueBodyCandidates(ctx, h.account.ID)
	if queued := len(supervisor.bodyQueue); queued != 0 {
		t.Fatalf("a capped message was queued again (%d in the queue)", queued)
	}
	// The cap must not have quietly moved the row out of the candidate state either:
	// the whole point is that the row stays as it is and is skipped.
	if state := bodyState(t, h, id); state != "error" {
		t.Fatalf("body_state = %q after a skipped candidate", state)
	}

	// One attempt below the cap it is still eligible, so the cap is not off by one.
	supervisor.bodyAttempts.Store(id, maxBodyAttempts-1)
	supervisor.enqueueBodyCandidates(ctx, h.account.ID)
	if queued := len(supervisor.bodyQueue); queued != 1 {
		t.Fatalf("a message below the cap produced %d queue entries, want 1", queued)
	}
	if got := <-supervisor.bodyQueue; got != id {
		t.Fatalf("queued id = %d, want %d", got, id)
	}

	// A message with no recorded failures is eligible as well, which is the ordinary
	// path the cap must not disturb.
	supervisor.bodyAttempts.Delete(id)
	supervisor.bodySeen.Delete(id)
	if err := h.repo.SetMessageBodyState(ctx, id, "error"); err != nil {
		t.Fatal(err)
	}
	supervisor.enqueueBodyCandidates(ctx, h.account.ID)
	if queued := len(supervisor.bodyQueue); queued != 1 {
		t.Fatalf("an untried message produced %d queue entries, want 1", queued)
	}
}

// seedBodyCandidate writes a message that the candidate query selects: incoming, not
// ready, and with a known size. Written through the repository rather than fetched
// over IMAP so the queue has exactly one candidate and no loop competes for it.
func seedBodyCandidate(t *testing.T, h *harness) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	mailbox := domain.Mailbox{
		AccountID: h.account.ID, RemoteName: "INBOX", DisplayName: "Inbox", Role: "inbox",
		SyncMode: "realtime", UIDValidity: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := h.repo.UpsertMailbox(ctx, &mailbox); err != nil {
		t.Fatal(err)
	}
	// UpsertMailbox writes through raw SQL and leaves the struct id at zero, so the
	// real id has to be read back before it can be used as a foreign key.
	stored, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("body candidate"))
	message := domain.Message{
		AccountID: h.account.ID, Direction: "incoming", DedupeKey: digest[:], Subject: "body candidate",
		Sender: "sender@example.com", Recipients: "mail@example.com",
		FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
		BodyState: "error", SizeBytes: 512, ReceivedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	created, err := h.repo.CreateOrUpdateMessage(ctx, &message, stored.ID, 1, nil, time.UnixMilli(now))
	if err != nil || !created {
		t.Fatalf("seed message: created=%v err=%v", created, err)
	}
	if err := h.repo.SetMessageBodyState(ctx, message.ID, "error"); err != nil {
		t.Fatal(err)
	}
	return message.ID
}

func bodyState(t *testing.T, h *harness, id int64) string {
	t.Helper()
	message, _, err := h.repo.GetMessage(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return message.BodyState
}

func attempts(t *testing.T, supervisor *Supervisor, id int64) int {
	t.Helper()
	value, ok := supervisor.bodyAttempts.Load(id)
	if !ok {
		return 0
	}
	count, valid := value.(int)
	if !valid {
		t.Fatalf("bodyAttempts holds %T", value)
	}
	return count
}

// TestCommandLoopMarksAuthError covers the second half of the credential fix. Wrong
// credentials used to be indistinguishable from a network fault, so the loop retried
// on the 1s ladder and hammered the provider with failing logins — which is how an
// account gets locked out. The account now reports auth_error and waits.
func TestCommandLoopMarksAuthError(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Second account, same mailbox, deliberately wrong password.
	wrong, err := h.accounts.AddPassword(ctx, "qq", "other@example.com", "Wrong", "mail@example.com", "not-the-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(30 * time.Second)
	var status, lastError string
	for time.Now().Before(deadline) {
		account, err := h.repo.GetAccount(ctx, wrong.ID)
		if err == nil {
			status = account.Status
			if account.LastError != nil {
				lastError = *account.LastError
			}
			if status == "auth_error" {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if status != "auth_error" {
		t.Fatalf("status = %q, want auth_error (last_error=%q)", status, lastError)
	}
	if lastError == "" {
		t.Fatal("no error was recorded for the operator to act on")
	}
	// The password must not reach the log or the stored error.
	if strings.Contains(lastError, "not-the-password") {
		t.Fatalf("credential leaked into last_error: %q", lastError)
	}

	// The healthy account must still be syncing: one bad account cannot stall the rest.
	h.deliver(t, "healthy-account-still-syncs")
	h.events.await(t, "NEW_EMAIL", 60*time.Second)
}

// TestReconcileAppliesRemoteFlagsAndExpunges is the end-to-end version of the
// headline fix. Sync only ever appended new UIDs, so mail read, starred or deleted
// in another client stayed as it was here forever — the app disagreed with every
// other client looking at the same mailbox.
func TestReconcileAppliesRemoteFlagsAndExpunges(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.deliver(t, "read-elsewhere")
	h.deliver(t, "deleted-elsewhere")
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, func() bool {
		page, err := h.repo.ListMessages(ctx, ports.MessageFilter{Limit: 10})
		return err == nil && len(page.Items) == 2
	})

	// A second real connection, because "another client" is exactly the scenario: the
	// flags and the expunge have to arrive through the protocol, not through the store.
	other := h.connect(t, ctx)
	defer other.Close()
	if _, err := other.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Store(goimap.UIDSetNum(1), &goimap.StoreFlags{
		Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagSeen, goimap.FlagFlagged},
	}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Store(goimap.UIDSetNum(2), &goimap.StoreFlags{
		Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagDeleted},
	}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if err := other.Expunge().Close(); err != nil {
		t.Fatal(err)
	}

	// Reconciliation is throttled to one pass per interval, so the throttle is cleared
	// and a sync requested rather than waiting the interval out: what it does is under
	// test here, not when it runs.
	mailboxID := inboxMailboxID(t, h)
	waitFor(t, 60*time.Second, func() bool {
		h.supervisor.lastReconcile.Delete(mailboxID)
		if err := h.supervisor.RequestMailbox(ctx, mailboxID); err != nil {
			return false
		}
		page, err := h.repo.ListMessages(ctx, ports.MessageFilter{Limit: 10})
		if err != nil || len(page.Items) != 1 {
			return false
		}
		item := page.Items[0]
		return item.Subject == "read-elsewhere" && item.IsRead && item.IsStarred
	})

	// The count the badge shows has to follow: the remaining message is read, so the
	// view holds nothing unread.
	page, err := h.repo.ListMessages(ctx, ports.MessageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.UnreadTotal != 0 {
		t.Fatalf("UnreadTotal = %d after the only message was read elsewhere", page.UnreadTotal)
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("condition was not met before the deadline")
}

func inboxMailboxID(t *testing.T, h *harness) int64 {
	t.Helper()
	mailbox, err := h.repo.GetMailboxByRole(context.Background(), h.account.ID, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	return mailbox.ID
}
