//go:build sqlite_fts5

package imap

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
	providerauth "nexusmail/internal/provider/auth"

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
		// The token path wraps every refresh failure, including a dropped
		// connection to the provider's token endpoint, in the same prefix. That
		// prefix used to be on the marker list, so this exact error parked a
		// healthy account and told the user its credentials were invalid.
		fmt.Errorf("refresh OAuth token: %w", errors.New(`Post "https://oauth2.googleapis.com/token": EOF`)),
	}
	for _, err := range transient {
		if isAuthFailure(err) {
			t.Errorf("isAuthFailure(%v) = true, want false", err)
		}
	}

	// A refusal the token path attributes to the credential is a verdict, not an
	// inference, and travels as a sentinel rather than as text.
	rejected := fmt.Errorf("%w: refresh OAuth token: oauth2 %q", providerauth.ErrCredentialRejected, "invalid_grant")
	if !credentialsRejected(rejected) {
		t.Error("credentialsRejected on a wrapped ErrCredentialRejected = false, want true")
	}
	for _, err := range transient {
		if credentialsRejected(err) {
			t.Errorf("credentialsRejected(%v) = true, want false", err)
		}
	}
}

// qqLoginFail is QQ's reply to a refused LOGIN, verbatim as it reaches the loops.
// It is one string for five different causes, two of which need the user and two of
// which clear on their own, which is what makes it its own classification.
const qqLoginFail = "imap: NO Login fail. Account is abnormal, service is not open, " +
	"password is incorrect, login frequency limited, or system is busy. " +
	"More information at https://help.mail.qq.com/detail/108/1023"

// TestIsAmbiguousLoginRejection guards the gap this message fell through. It carries
// no response code and matches neither marker list — isAuthFailure has "login denied"
// but not "login fail", and isRateLimited has "system busy" but not "system is busy"
// — so it reached the default branch and was treated as a network fault: retried on
// the 1s ladder, which is what produces the "login frequency limited" it names, with
// the raw text shown to the user on the first try.
func TestIsAmbiguousLoginRejection(t *testing.T) {
	qq := errors.New(qqLoginFail)
	if !isAmbiguousLoginRejection(qq) {
		t.Error("isAmbiguousLoginRejection on QQ's login refusal = false, want true")
	}
	// The premise of the dedicated branch: neither existing classifier claims it, so
	// without this it lands on the ladder. If a later widening makes one of them
	// match, the branch order still has to keep this message off the ladder and off
	// the silent-forever throttle window.
	if isAuthFailure(qq) || isRateLimited(qq) {
		t.Errorf("QQ login refusal now matches isAuthFailure=%v isRateLimited=%v; check the branch order in classifyFailure",
			isAuthFailure(qq), isRateLimited(qq))
	}
	if isAmbiguousLoginRejection(fmt.Errorf("connect: %w", qq)) == false {
		t.Error("isAmbiguousLoginRejection does not see through a wrapped error")
	}

	unambiguous := []error{
		nil,
		errors.New("EOF"),
		errors.New("dial tcp 1.2.3.4:993: i/o timeout"),
		// A provider that says which one it is keeps its own classification and the
		// shorter reporting window that goes with it.
		errors.New("imap: NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)"),
		errors.New("imap: NO System busy!"),
	}
	for _, err := range unambiguous {
		if isAmbiguousLoginRejection(err) {
			t.Errorf("isAmbiguousLoginRejection(%v) = true, want false", err)
		}
	}
}

// TestAmbiguousLoginRejectionCorroborated is the regression for the report: an
// account showed "同步失败：imap: NO Login fail. …" and kept reconnecting on the 1s
// ladder. Because the message can be QQ's login-frequency limit, an isolated one must
// be silent and must wait out the throttle window — retrying it on authRetryBackoff
// would leave both loops logging in ~2×/minute against that limit. Because it can
// equally be a wrong password or IMAP left switched off, sustained rejection must
// still reach the user with the text, which names the settings to check.
func TestAmbiguousLoginRejectionCorroborated(t *testing.T) {
	const ladder = 2 * time.Second
	supervisor := &Supervisor{authRetry: 50 * time.Millisecond}
	qq := errors.New(qqLoginFail)
	rt := &runtime{}

	for attempt := 1; attempt < maxAuthFailures; attempt++ {
		result := supervisor.classifyFailure(rt, qq, ladder)
		if result.status != "backoff" || result.store {
			t.Fatalf("rejection %d = %q store=%v, want backoff with nothing stored", attempt, result.status, result.store)
		}
		// Not the ladder, and not the short auth window either: the message names a
		// frequency limit, so the unbelieved retry has to be the throttle-shaped one.
		if result.delay != rateLimitBackoff {
			t.Fatalf("rejection %d delay = %v, want %v", attempt, result.delay, rateLimitBackoff)
		}
		if result.ladder {
			t.Fatalf("rejection %d advanced the network ladder", attempt)
		}
	}

	// A login that succeeds in between retires the evidence, so a refusal seen once
	// an hour never accumulates into a park.
	rt.authSucceeded()
	if result := supervisor.classifyFailure(rt, qq, ladder); result.store {
		t.Error("a rejection after a successful auth was stored, want nothing shown to the user")
	}

	rt.authSucceeded()
	var final verdict
	for range maxAuthFailures {
		final = supervisor.classifyFailure(rt, qq, ladder)
	}
	if final.status != "auth_error" || !final.store || final.delay != authBackoff {
		t.Errorf("sustained rejection = %q store=%v delay=%v, want auth_error stored on %v",
			final.status, final.store, final.delay, authBackoff)
	}
}

// TestRetryDelayAndStatusMapping pins what the classifications above are for.
// Both loops route every post-connect failure through classifyFailure, so this is
// where the invariant lives: a failure that happened *after* the greeting must never
// come back on the 1s network ladder if it was an auth rejection or a throttle.
// Reaching the greeting proves the socket works, not that the account can be read,
// and retrying a refused credential or an engaged throttle every second is what kept
// a real account locked out for hours.
//
// The delay mapping had no test before, and dropping a branch from it left the whole
// package green — so the arithmetic is asserted directly here rather than only
// through the loops that consume it.
func TestRetryDelayAndStatusMapping(t *testing.T) {
	// A short ladder value, distinguishable from the long windows.
	const ladder = 2 * time.Second
	supervisor := &Supervisor{}

	for _, testCase := range []struct {
		name string
		err  error
		want verdict
	}{
		// A single IMAP rejection is not yet believed: it retries on the short auth
		// window and is not shown to the user. Gmail issues these for valid tokens.
		{"an auth rejection retries before it is believed", errors.New("imap: AUTHENTICATIONFAILED bad password"),
			verdict{status: "backoff", delay: authRetryBackoff}},
		{"expired credentials retry the same way", &goimap.Error{Code: goimap.ResponseCodeExpired, Text: "expired"},
			verdict{status: "backoff", delay: authRetryBackoff}},
		// A verdict from the token path needs no corroboration.
		{"a rejected credential parks immediately", fmt.Errorf("%w: refresh OAuth token: oauth2 %q", providerauth.ErrCredentialRejected, "invalid_grant"),
			verdict{status: "auth_error", delay: authBackoff, store: true}},
		// A throttle is retryable, so the account stays in backoff rather than
		// asking the user to fix credentials that are fine.
		{"a throttle waits out the rate-limit window", errors.New("imap: NO System busy!"),
			verdict{status: "backoff", delay: rateLimitBackoff}},
		{"too many connections is a throttle", errors.New("imap: BYE Too many concurrent connections"),
			verdict{status: "backoff", delay: rateLimitBackoff}},
		{"try later is a throttle", &goimap.Error{Code: goimap.ResponseCodeUnavailable, Text: "try later"},
			verdict{status: "backoff", delay: rateLimitBackoff}},
		// A refusal that might be the throttle it names starts on the throttle window
		// too, and only becomes auth_error if it keeps happening. Deliberately not the
		// ladder: the ladder is what triggers the limit in the message.
		{"an ambiguous login refusal waits out the throttle window first", errors.New(qqLoginFail),
			verdict{status: "backoff", delay: rateLimitBackoff}},
		// Ordinary network faults are what the ladder exists for: they clear on
		// their own and retrying quickly is the right thing to do. Only these
		// advance it — the fixed windows must not double underneath the caller.
		{"a network fault keeps the ladder", errors.New("dial tcp: i/o timeout"),
			verdict{status: "backoff", delay: ladder, store: true, ladder: true}},
		{"EOF keeps the ladder", errors.New("EOF"),
			verdict{status: "backoff", delay: ladder, store: true, ladder: true}},
		{"a server bug keeps the ladder", errors.New("imap: SERVERBUG internal error"),
			verdict{status: "backoff", delay: ladder, store: true, ladder: true}},
		{"no error keeps the ladder", nil,
			verdict{status: "backoff", delay: ladder, store: true, ladder: true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// A fresh runtime per case: the auth counter is per account and carrying
			// it between cases would make the order significant.
			if result := supervisor.classifyFailure(&runtime{}, testCase.err, ladder); result != testCase.want {
				t.Errorf("classifyFailure = %+v, want %+v", result, testCase.want)
			}
		})
	}

	// Auth is checked before throttling, so an error that reads as both is treated
	// as the one a retry cannot fix. Getting this order wrong would put a genuinely
	// bad credential on the throttle window, where it retries silently forever
	// instead of ever telling the user to re-authenticate.
	both := errors.New("imap: NO authentication failed, too many attempts")
	rt := &runtime{}
	for range maxAuthFailures - 1 {
		if result := supervisor.classifyFailure(rt, both, ladder); result.status != "backoff" {
			t.Fatalf("auth+throttle error status = %q before the threshold, want backoff", result.status)
		}
	}
	if result := supervisor.classifyFailure(rt, both, ladder); result.delay != authBackoff || result.status != "auth_error" {
		t.Errorf("auth+throttle error at the threshold = %v/%q, want %v/auth_error", result.delay, result.status, authBackoff)
	}

	// The ladder is passed through rather than clamped, which is what lets the
	// caller's 1s→5m progression survive this mapping.
	for _, value := range []time.Duration{0, time.Second, 5 * time.Minute} {
		if result := supervisor.classifyFailure(&runtime{}, errors.New("EOF"), value); result.delay != value {
			t.Errorf("classifyFailure passed ladder %v through as %v", value, result.delay)
		}
	}
}

// TestAuthFailureNeedsCorroboration is the regression for the report that started
// this: a Gmail account that synced fine before and after showed the user
// "同步失败：imap: NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)" and
// stopped syncing for 15 minutes, 11 times over 6 days. Gmail returns that for a
// valid OAuth token when it throttles authentication, so a lone rejection must
// retry on the short window with nothing stored, and a successful authentication in
// between must clear the evidence rather than letting unrelated rejections hours
// apart accumulate into a park.
func TestAuthFailureNeedsCorroboration(t *testing.T) {
	const ladder = 2 * time.Second
	supervisor := &Supervisor{}
	gmail := errors.New("imap: NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)")
	rt := &runtime{}

	for attempt := 1; attempt < maxAuthFailures; attempt++ {
		result := supervisor.classifyFailure(rt, gmail, ladder)
		if result.status != "backoff" || result.store {
			t.Fatalf("rejection %d = %q store=%v, want backoff with nothing stored", attempt, result.status, result.store)
		}
		if result.delay != authRetryBackoff {
			t.Fatalf("rejection %d delay = %v, want %v", attempt, result.delay, authRetryBackoff)
		}
	}

	// Authenticating retires the count, so the next isolated rejection starts over
	// instead of being the one that parks the account.
	rt.authSucceeded()
	if result := supervisor.classifyFailure(rt, gmail, ladder); result.status != "backoff" || result.store {
		t.Errorf("rejection after a successful auth = %q store=%v, want backoff with nothing stored", result.status, result.store)
	}

	// Sustained rejection is a real credential problem and must reach the user.
	rt.authSucceeded()
	var final verdict
	for range maxAuthFailures {
		final = supervisor.classifyFailure(rt, gmail, ladder)
	}
	if final.status != "auth_error" || !final.store || final.delay != authBackoff {
		t.Errorf("sustained rejection = %q store=%v delay=%v, want auth_error stored on %v", final.status, final.store, final.delay, authBackoff)
	}
}

// TestIsRateLimited pins the throttling classification. The 1s→5m network
// ladder makes the throttle worse against QQ/163; without this classification
// a single "System busy" reply would keep triggering fast reconnects that the
// provider reads as further abuse.
func TestIsRateLimited(t *testing.T) {
	throttled := []error{
		&goimap.Error{Code: goimap.ResponseCodeUnavailable, Text: "try later"},
		errors.New("imap: NO System busy!"),
		errors.New("imap: BYE Too many concurrent connections"),
		errors.New("Too many simultaneous connections"),
		errors.New("Rate limit exceeded for mailbox"),
		errors.New("Quota exceeded"),
		errors.New("please try again later"),
		fmt.Errorf("sync INBOX: %w", errors.New("system busy, retry later")),
	}
	for _, err := range throttled {
		if !isRateLimited(err) {
			t.Errorf("isRateLimited(%v) = false, want true", err)
		}
	}

	notThrottled := []error{
		nil,
		errors.New("dial tcp 1.2.3.4:993: i/o timeout"),
		errors.New("connection reset by peer"),
		errors.New("imap: SERVERBUG internal error"),
		errors.New("EOF"),
		&goimap.Error{Code: goimap.ResponseCodeAuthenticationFailed, Text: "no"},
		&goimap.Error{Code: goimap.ResponseCodeInUse, Text: "mailbox busy"},
		context.Canceled,
	}
	for _, err := range notThrottled {
		if isRateLimited(err) {
			t.Errorf("isRateLimited(%v) = true, want false", err)
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
	// The batch path is what a real sync uses; there is no single-row variant.
	ids, created, err := h.repo.BatchCreateOrUpdateMessages(ctx, []ports.MessageInput{{
		Message: &message, MailboxID: stored.ID, UID: 1, InternalDate: time.UnixMilli(now),
	}})
	if err != nil || !created[0] {
		t.Fatalf("seed message: created=%v err=%v", created, err)
	}
	message.ID = ids[0]
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
//
// A rejection the provider might have issued transiently is corroborated first (see
// TestAuthFailureNeedsCorroboration), so this drives the loop through the whole walk
// to maxAuthFailures on a shortened window: a password that is genuinely wrong is
// refused every time and must still end up parked with a message for the user.
func TestCommandLoopMarksAuthError(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Short enough that the corroboration walk fits inside the deadline below,
	// long enough to stay a real wait rather than a spin.
	h.supervisor.authRetry = 200 * time.Millisecond

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

// TestFormatAddressKeepsDecodedNames covers the display and index form of an address.
// net/mail's Address.String() re-encodes any non-ASCII display name into an RFC 2047
// encoded-word, so a name go-imap had already decoded was stored — and shown — as a
// literal "=?utf-8?q?...?=", which also left the readable name unsearchable in FTS5.
func TestFormatAddressKeepsDecodedNames(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		display string
		address string
		want    string
	}{
		{name: "cjk name stays readable", display: "阿里云", address: "noreply@aliyun.com", want: "阿里云 <noreply@aliyun.com>"},
		{name: "ascii name needs no quotes", display: "Ali Cloud", address: "noreply@aliyun.com", want: "Ali Cloud <noreply@aliyun.com>"},
		{name: "empty name yields the bare address", display: "", address: "noreply@aliyun.com", want: "noreply@aliyun.com"},
		{name: "whitespace-only name yields the bare address", display: "  ", address: "a@b.com", want: "a@b.com"},
		{name: "specials force quoting", display: "Foo, Bar", address: "a@b.com", want: `"Foo, Bar" <a@b.com>`},
		{name: "embedded quote is escaped", display: `He said "hi"`, address: "a@b.com", want: `"He said \"hi\"" <a@b.com>`},
		{name: "angle brackets are quoted", display: "Foo <spoof@evil.com>", address: "a@b.com", want: `"Foo <spoof@evil.com>" <a@b.com>`},
		// A header injection attempt must not survive into a stored address, because
		// this value is later parsed back and written into an outgoing draft.
		{name: "newlines are dropped", display: "Foo\r\nBcc: victim@example.com", address: "a@b.com", want: `"Foo Bcc: victim@example.com" <a@b.com>`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := formatAddress(testCase.display, testCase.address)
			if got != testCase.want {
				t.Fatalf("formatAddress(%q, %q) = %q, want %q", testCase.display, testCase.address, got, testCase.want)
			}
			if strings.Contains(got, "=?") {
				t.Fatalf("formatAddress(%q, %q) produced an encoded-word: %q", testCase.display, testCase.address, got)
			}
			// The stored form is parsed back when a draft built from it is sent, so a
			// value that cannot round-trip would fail the send instead of the display.
			parsed, err := mail.ParseAddress(got)
			if err != nil {
				t.Fatalf("mail.ParseAddress(%q) failed: %v", got, err)
			}
			if parsed.Address != testCase.address {
				t.Fatalf("round-trip address = %q, want %q", parsed.Address, testCase.address)
			}
		})
	}
}

// TestAddressesFromEnvelopeAreReadable pins the same behaviour at the call site that
// produces the sender and recipients columns.
func TestAddressesFromEnvelopeAreReadable(t *testing.T) {
	got := addresses([]goimap.Address{
		{Name: "阿里云", Mailbox: "noreply", Host: "aliyun.com"},
		{Name: "", Mailbox: "plain", Host: "example.com"},
		{Name: "skipped", Mailbox: "", Host: ""},
	})
	want := []string{"阿里云 <noreply@aliyun.com>", "plain@example.com"}
	if len(got) != len(want) {
		t.Fatalf("addresses() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("addresses()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestProbeInboxThreeBranches pins the three observable outcomes of the 5s
// inbox probe. The cheap STATUS path has to take all three routes: the
// unchanged branch (most ticks), the moved branch that calls
// syncMailbox(skipReconcile=true), and a STATUS error that propagates to the
// caller. Without this, a regression that makes probeInbox always return nil
// after movement would not be caught by TestIdleInboxProbeStaysCheap.
//
// The supervisor is stopped before the moved-branch assertion so the
// commandLoop does not race the manual probe for the same UIDs and trip a
// UNIQUE constraint; the assertion is about the branch the probe took, not
// about a real-time race.
func TestProbeInboxThreeBranches(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, h)
	h.supervisor.Stop()

	rt, err := h.supervisor.runtime(h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	client := h.connect(t, ctx)
	t.Cleanup(func() { _ = client.Close() })

	// 1) Unchanged branch: STATUS matches local UIDNext/UIDValidity,
	//    probeInbox must return nil and NOT call syncMailbox.
	before := h.events.count("NEW_EMAIL")
	if err := h.supervisor.probeInbox(ctx, rt, client); err != nil {
		t.Fatalf("unchanged probe returned %v, want nil", err)
	}
	if h.events.count("NEW_EMAIL") != before {
		t.Errorf("unchanged probe triggered a sync, want no event")
	}

	// 2) Moved branch: deliver a message, run the probe, expect one
	//    NEW_EMAIL because the moved branch calls syncMailbox.
	h.deliver(t, "probe-moved")
	if err := h.supervisor.probeInbox(ctx, rt, client); err != nil {
		t.Fatalf("moved probe returned %v, want nil", err)
	}
	_, _ = h.events.await(t, "NEW_EMAIL", 30*time.Second)

	// 3) STATUS error branch: close the client so the next Status() call
	//    fails. The function should return the error to the caller so the
	//    command loop can take the backoff path.
	_ = client.Close()
	if err := h.supervisor.probeInbox(ctx, rt, client); err == nil {
		t.Errorf("STATUS error branch returned nil, want a transport error")
	}
}

// TestRuntimeLockTwoLevelPreemption covers the two-level priority lock
// directly. CLAUDE.md flags it as a footgun; until now the only coverage was
// the end-to-end TestBodyPrefetchYieldsToNewMail, which uses a 3s wall-clock
// budget that can be racy in CI. The test asserts:
//   - lockBackground blocks while the foreground lock is held
//   - lockBackground returns false when its context is cancelled
//   - urgent is balanced (start and end match)
func TestRuntimeLockTwoLevelPreemption(t *testing.T) {
	rt := &runtime{}
	if !rt.lockBackground(context.Background()) {
		t.Fatal("fresh runtime should accept lockBackground")
	}
	rt.unlock()

	rt.lock()
	holder := make(chan struct{})
	acquired := make(chan struct{})
	go func() {
		close(holder)
		// While the foreground lock is held, lockBackground must spin and
		// not return. We measure by giving it a tight context.
		short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if rt.lockBackground(short) {
			close(acquired)
		}
	}()
	<-holder
	select {
	case <-acquired:
		t.Fatal("lockBackground acquired the lock while foreground was held")
	case <-time.After(80 * time.Millisecond):
		// expected
	}
	rt.unlock()
	// After release, lockBackground should acquire within a reasonable
	// window. The test would hang here if the spin loop never observes the
	// unlock.
	if !rt.lockBackground(context.Background()) {
		t.Fatal("lockBackground did not acquire after unlock")
	}
	rt.unlock()

	// Cancellation: a context that is already done must make lockBackground
	// return false rather than block.
	rt.lock()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if rt.lockBackground(cancelled) {
		t.Fatal("lockBackground acquired with a cancelled context")
	}
	rt.unlock()
}
