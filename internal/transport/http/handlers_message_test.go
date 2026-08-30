//go:build sqlite_fts5

package http

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
)

// messageFixture is one account with an inbox and an archive, plus n messages
// whose received_at is strictly ordered so cursor assertions are deterministic.
type messageFixture struct {
	account domain.Account
	inbox   domain.Mailbox
	archive domain.Mailbox
	ids     []int64
}

func (h *harness) seedFeed(count int) messageFixture {
	h.t.Helper()
	fixture := messageFixture{account: h.seedAccount()}
	fixture.inbox = h.seedMailbox(fixture.account.ID, "INBOX", "inbox")
	fixture.archive = h.seedMailbox(fixture.account.ID, "Archive", "archive")
	base := time.Now().UnixMilli() - int64(count)*1000
	for i := 0; i < count; i++ {
		fixture.ids = append(fixture.ids, h.seedMessage(fixture.account.ID, fixture.inbox.ID, uint32(i+1), seedOptions{
			subject:  fmt.Sprintf("Message %02d", i),
			received: base + int64(i)*1000,
		}))
	}
	return fixture
}

func (h *harness) listMessagePage(query string) ports.MessagePage {
	h.t.Helper()
	response := h.do(http.MethodGet, "/api/v1/messages"+query, nil)
	if response.Code != http.StatusOK {
		h.t.Fatalf("GET /messages%s = %d: %s", query, response.Code, response.Body.String())
	}
	return decodeBody[ports.MessagePage](h.t, response)
}

func TestListMessagesNewestFirst(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(3)
	page := h.listMessagePage("")
	if len(page.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(page.Items))
	}
	if page.Items[0].Subject != "Message 02" || page.Items[2].Subject != "Message 00" {
		t.Fatalf("order = %q, %q, %q", page.Items[0].Subject, page.Items[1].Subject, page.Items[2].Subject)
	}
	// Every seeded message is unread, and the total counts the view rather than the page.
	if page.UnreadTotal != 3 {
		t.Fatalf("unread_total = %d, want 3", page.UnreadTotal)
	}
	if page.NextCursor != "" {
		t.Fatalf("next_cursor = %q on a complete page", page.NextCursor)
	}
	_ = fixture
}

// The keyset cursor must walk the whole feed exactly once: no repeats, no gaps.
// An OFFSET pager would repeat or skip rows as new mail arrives; this asserts the
// contract that makes that impossible.
func TestListMessagesCursorPaginatesWithoutOverlap(t *testing.T) {
	h := newHarness(t)
	h.seedFeed(25)

	seen := map[int64]int{}
	var order []int64
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		query := "?limit=7"
		if cursor != "" {
			query += "&cursor=" + cursor
		}
		page := h.listMessagePage(query)
		for _, item := range page.Items {
			seen[item.ID]++
			order = append(order, item.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(order) != 25 {
		t.Fatalf("walked %d messages, want 25", len(order))
	}
	for id, times := range seen {
		if times != 1 {
			t.Fatalf("message %d appeared %d times", id, times)
		}
	}
	for i := 1; i < len(order); i++ {
		if order[i] >= order[i-1] {
			t.Fatalf("order broke at %d: %d then %d", i, order[i-1], order[i])
		}
	}
}

// A cursor from a different sort order, a truncated one, or one that is not even
// base64 must be rejected rather than silently restarting the feed at the top —
// a client that retried with a corrupted cursor would otherwise re-deliver page one
// forever.
func TestListMessagesRejectsABrokenCursor(t *testing.T) {
	h := newHarness(t)
	h.seedFeed(2)
	for _, cursor := range []string{"not-base64!!", base64.StdEncoding.EncodeToString([]byte("{")), base64.StdEncoding.EncodeToString([]byte(`{"received_at":"nope"}`))} {
		response := h.do(http.MethodGet, "/api/v1/messages?cursor="+cursor, nil)
		if response.Code == http.StatusOK {
			t.Fatalf("cursor %q was accepted", cursor)
		}
	}
}

func TestListMessagesFilters(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(4)
	other := h.seedAccount2()
	otherInbox := h.seedMailbox(other.ID, "INBOX", "inbox")
	h.seedMessage(other.ID, otherInbox.ID, 1, seedOptions{subject: "Other account"})
	// One archived message, and one already read.
	h.seedMessage(fixture.account.ID, fixture.archive.ID, 99, seedOptions{subject: "Archived note"})
	read := h.do(http.MethodPatch, fmt.Sprintf("/api/v1/messages/%d", fixture.ids[0]), map[string]any{"is_read": true})
	if read.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", read.Code, read.Body.String())
	}

	cases := []struct {
		query string
		want  int
	}{
		{"", 6},
		{"?account_id=" + fmt.Sprint(fixture.account.ID), 5},
		{"?account_id=" + fmt.Sprint(other.ID), 1},
		{"?mailbox_id=" + fmt.Sprint(fixture.inbox.ID), 4},
		{"?folder=archive", 1},
		{"?folder=inbox", 5},
		{"?is_read=true", 1},
		{"?is_read=false", 5},
		{"?query=Archived", 1},
		{"?query=Message", 4},
		{"?folder=inbox&is_read=false&account_id=" + fmt.Sprint(fixture.account.ID), 3},
	}
	for _, tc := range cases {
		if got := len(h.listMessagePage(tc.query).Items); got != tc.want {
			t.Errorf("GET /messages%s returned %d items, want %d", tc.query, got, tc.want)
		}
	}
}

func (h *harness) seedAccount2() domain.Account {
	h.t.Helper()
	now := time.Now().UnixMilli()
	account := domain.Account{
		Email: "second@example.com", DisplayName: "Second", Provider: "163", AuthType: "password",
		Username: "second@example.com", IMAPHost: "imap.163.com", IMAPPort: 993, IMAPTLSMode: "implicit",
		SMTPHost: "smtp.163.com", SMTPPort: 465, SMTPTLSMode: "implicit",
		SecretCiphertext: []byte("sealed-credential"), Status: "connected", CreatedAt: now, UpdatedAt: now,
	}
	if err := h.repo.CreateAccount(context.Background(), &account); err != nil {
		h.t.Fatal(err)
	}
	return account
}

// A search term is not a wildcard: an unmatched term returns nothing rather than
// falling back to the unfiltered feed.
func TestListMessagesSearchIsNotAWildcard(t *testing.T) {
	h := newHarness(t)
	h.seedFeed(3)
	if got := len(h.listMessagePage("?query=nothingmatchesthis").Items); got != 0 {
		t.Fatalf("items = %d, want 0", got)
	}
}

// Search input goes into an FTS5 MATCH and a LIKE. Neither may let a caller break
// out of the expression — a syntax error there would surface as a 500 and, worse,
// prove the term is not being bound.
func TestListMessagesSearchHandlesHostileInput(t *testing.T) {
	h := newHarness(t)
	h.seedFeed(2)
	hostile := []string{
		`" OR 1=1 --`, `%`, `_`, `\`, `'`, `);DROP TABLE messages;--`,
		`NEAR(`, `subject:`, `*`, `""`, strings.Repeat("a", 300), `中文 测试`,
	}
	for _, term := range hostile {
		response := h.do(http.MethodGet, "/api/v1/messages?query="+url.QueryEscape(term), nil)
		if response.Code != http.StatusOK && response.Code != http.StatusBadRequest {
			t.Errorf("query %q returned %d: %s", term, response.Code, response.Body.String())
		}
	}
	// The table is still there and still has both rows.
	if got := len(h.listMessagePage("").Items); got != 2 {
		t.Fatalf("items = %d after hostile queries, want 2", got)
	}
}

func TestListMessagesRejectsBadFilters(t *testing.T) {
	h := newHarness(t)
	h.expectError(h.do(http.MethodGet, "/api/v1/messages?is_read=maybe", nil), 400, "invalid_filter")
	h.expectError(h.do(http.MethodGet, "/api/v1/messages?account_id=abc", nil), 400, "invalid_filter")
	h.expectError(h.do(http.MethodGet, "/api/v1/messages?account_id=0", nil), 400, "invalid_filter")
	h.expectError(h.do(http.MethodGet, "/api/v1/messages?mailbox_id=-3", nil), 400, "invalid_filter")
}

// The limit is clamped rather than trusted: a caller asking for 100000 rows must
// not be able to make one request read the whole table.
func TestListMessagesClampsTheLimit(t *testing.T) {
	h := newHarness(t)
	h.seedFeed(30)
	for _, query := range []string{"?limit=100000", "?limit=-5", "?limit=abc", "?limit=0"} {
		page := h.listMessagePage(query)
		if len(page.Items) > 100 {
			t.Fatalf("limit%s returned %d items", query, len(page.Items))
		}
		if len(page.Items) == 0 {
			t.Fatalf("limit%s returned nothing", query)
		}
	}
}

// Opening a mailbox view asks the supervisor to refresh it, which is what makes a
// folder the user just clicked sync now rather than at the next 5-minute pass.
func TestListMessagesRequestsTheMailboxSync(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(1)
	h.listMessagePage("?mailbox_id=" + fmt.Sprint(fixture.inbox.ID))
	if _, mailbox, _, _ := h.provider.counts(); mailbox != 1 {
		t.Fatalf("RequestMailbox called %d times, want 1", mailbox)
	}
	// Without a mailbox filter there is nothing to refresh.
	h.listMessagePage("")
	if _, mailbox, _, _ := h.provider.counts(); mailbox != 1 {
		t.Fatalf("RequestMailbox called %d times after an unfiltered list", mailbox)
	}
}

// A provider that cannot refresh must not fail the read: the local rows are still
// worth serving.
func TestListMessagesSurvivesASyncFailure(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(2)
	h.provider.set(func(f *fakeProvider) { f.mailboxErr = ports.Unavailablef("account offline") })
	if got := len(h.listMessagePage("?mailbox_id=" + fmt.Sprint(fixture.inbox.ID)).Items); got != 2 {
		t.Fatalf("items = %d, want 2", got)
	}
}

// --- detail ---------------------------------------------------------------

func TestGetMessageReturnsBodyAndAttachments(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(1)
	id := fixture.ids[0]
	if err := h.repo.BatchUpsertAttachments(context.Background(), []domain.Attachment{{
		MessageID: id, PartID: "2", Filename: "report.pdf", ContentType: "application/pdf",
		Disposition: "attachment", SizeBytes: 1024, FetchState: "metadata",
		CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}}); err != nil {
		t.Fatal(err)
	}
	response := h.do(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", id), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	payload := decodeBody[struct {
		Message     domain.Message      `json:"message"`
		Attachments []domain.Attachment `json:"attachments"`
		OTPCode     string              `json:"otp_code"`
	}](t, response)
	if payload.Message.ID != id {
		t.Fatalf("message id = %d, want %d", payload.Message.ID, id)
	}
	if len(payload.Attachments) != 1 || payload.Attachments[0].Filename != "report.pdf" {
		t.Fatalf("attachments = %+v", payload.Attachments)
	}
	// A body that is already ready needs no provider round trip.
	if _, _, body, _ := h.provider.counts(); body != 0 {
		t.Fatalf("FetchBody called %d times for a ready body", body)
	}
}

// A one-time code is derived on read, and only on the detail endpoint.
func TestGetMessageDerivesTheOTP(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	inbox := h.seedMailbox(account.ID, "INBOX", "inbox")
	id := h.seedMessage(account.ID, inbox.ID, 1, seedOptions{
		subject:  "Your verification code",
		bodyText: "Your verification code is 481920. It expires in 5 minutes.",
	})
	response := h.do(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", id), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	payload := decodeBody[struct {
		OTPCode string `json:"otp_code"`
	}](t, response)
	if payload.OTPCode != "481920" {
		t.Fatalf("otp_code = %q, want 481920", payload.OTPCode)
	}
	// The feed must not carry it: deriving one code per row would run the detector
	// over a whole page of bodies.
	if strings.Contains(h.do(http.MethodGet, "/api/v1/messages", nil).Body.String(), "otp_code") {
		t.Fatal("the feed carries otp_code")
	}
}

// A pending body is fetched inline when the provider is quick, and the refreshed
// row is what the client gets — not the placeholder it asked about.
func TestGetMessageFetchesAPendingBody(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	inbox := h.seedMailbox(account.ID, "INBOX", "inbox")
	id := h.seedMessage(account.ID, inbox.ID, 1, seedOptions{subject: "Pending", bodyState: "metadata"})
	h.provider.set(func(f *fakeProvider) {
		f.bodyEffect = func(messageID int64) {
			if err := h.repo.UpdateMessageBody(context.Background(), messageID, "fetched body", "", "ready", nil); err != nil {
				t.Error(err)
			}
		}
	})
	response := h.do(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", id), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	payload := decodeBody[struct {
		Message domain.Message `json:"message"`
	}](t, response)
	if payload.Message.BodyText != "fetched body" || payload.Message.BodyState != "ready" {
		t.Fatalf("message = %+v", payload.Message)
	}
}

// A provider failure answers 202 with the row as it stands: the client polls
// rather than being handed a 5xx for mail that does exist.
func TestGetMessageAnswers202WhenTheBodyCannotBeFetched(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	inbox := h.seedMailbox(account.ID, "INBOX", "inbox")
	id := h.seedMessage(account.ID, inbox.ID, 1, seedOptions{subject: "Pending", bodyState: "metadata"})
	h.provider.set(func(f *fakeProvider) { f.bodyErr = ports.Unavailablef("account offline") })
	response := h.do(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", id), nil)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", response.Code, response.Body.String())
	}
	if decodeBody[struct {
		Message domain.Message `json:"message"`
	}](t, response).Message.ID != id {
		t.Fatal("the 202 body does not carry the message")
	}
}

// The inline fetch is bounded: a provider that hangs turns into a 202 rather than
// holding the request open. The fetch keeps running in the background, which is
// why it is given the app context and not the request's.
func TestGetMessageDoesNotWaitForeverOnTheBody(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	inbox := h.seedMailbox(account.ID, "INBOX", "inbox")
	id := h.seedMessage(account.ID, inbox.ID, 1, seedOptions{subject: "Slow", bodyState: "metadata"})
	h.provider.set(func(f *fakeProvider) { f.bodyDelay = 10 * time.Second })
	start := time.Now()
	response := h.do(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", id), nil)
	elapsed := time.Since(start)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.Code)
	}
	if elapsed > 6*time.Second {
		t.Fatalf("the handler waited %s for a hanging provider", elapsed)
	}
}

// gin.Recovery only covers the request goroutine. A panic in the background body
// fetch has to be contained by the handler's own recover, or one malformed message
// takes the whole process down.
func TestGetMessageContainsAPanicInTheBodyFetch(t *testing.T) {
	h := newHarness(t)
	account := h.seedAccount()
	inbox := h.seedMailbox(account.ID, "INBOX", "inbox")
	id := h.seedMessage(account.ID, inbox.ID, 1, seedOptions{subject: "Boom", bodyState: "metadata"})
	h.provider.set(func(f *fakeProvider) { f.bodyPanic = true })
	response := h.do(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", id), nil)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.Code)
	}
	// The process is still serving.
	if h.plain(http.MethodGet, "/healthz").Code != http.StatusOK {
		t.Fatal("the server stopped answering after the panic")
	}
}

func TestGetMessageNotFound(t *testing.T) {
	h := newHarness(t)
	h.expectError(h.do(http.MethodGet, "/api/v1/messages/9999", nil), 404, "not_found")
}

// --- patch ----------------------------------------------------------------

func TestPatchMessageFlags(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(1)
	id := fixture.ids[0]
	response := h.do(http.MethodPatch, fmt.Sprintf("/api/v1/messages/%d", id), map[string]any{"is_read": true, "is_starred": true})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	message := decodeBody[domain.Message](t, response)
	if !message.IsRead || !message.IsStarred {
		t.Fatalf("message = %+v", message)
	}
	// The provider was updated before the local row, so the two cannot diverge.
	h.provider.mu.Lock()
	flagCalls := len(h.provider.flagCalls)
	h.provider.mu.Unlock()
	if flagCalls != 1 {
		t.Fatalf("SetFlags called %d times, want 1", flagCalls)
	}
	stored, _, err := h.repo.GetMessage(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.IsRead || !stored.IsStarred {
		t.Fatalf("stored = %+v", stored)
	}
}

// A provider that refuses the flag change must leave the local row untouched: a
// row that said "read" while the server said "unread" would show differently in
// every other client.
func TestPatchMessageDoesNotWriteLocallyWhenTheProviderFails(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(1)
	id := fixture.ids[0]
	h.provider.set(func(f *fakeProvider) { f.flagErr = ports.Unavailablef("account offline") })
	h.expectError(h.do(http.MethodPatch, fmt.Sprintf("/api/v1/messages/%d", id), map[string]any{"is_read": true}), 503, "provider_unavailable")
	stored, _, err := h.repo.GetMessage(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.IsRead {
		t.Fatal("the local row was marked read although the provider refused")
	}
}

// An empty patch is the caller's mistake, so it gets a 400 that says so rather
// than a redacted 500.
func TestPatchMessageRejectsAnEmptyPatch(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(1)
	envelope := h.expectError(h.do(http.MethodPatch, fmt.Sprintf("/api/v1/messages/%d", fixture.ids[0]), map[string]any{}), 400, "invalid_request")
	if !strings.Contains(envelope.Error.Message, "empty message patch") {
		t.Fatalf("message = %q", envelope.Error.Message)
	}
}

func TestPatchMessageArchive(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(1)
	response := h.do(http.MethodPatch, fmt.Sprintf("/api/v1/messages/%d", fixture.ids[0]), map[string]any{"archive": true})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	h.provider.mu.Lock()
	archived := append([]int64(nil), h.provider.archived...)
	h.provider.mu.Unlock()
	if len(archived) != 1 || archived[0] != fixture.ids[0] {
		t.Fatalf("archived = %v", archived)
	}
}

func TestPatchMessageRejectsMalformedJSON(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(1)
	request := newJSONRequest(http.MethodPatch, fmt.Sprintf("/api/v1/messages/%d", fixture.ids[0]), `{"is_read": "yes"}`)
	h.expectError(h.doRaw(request), 400, "invalid_request")
}

func TestPatchMessageNotFound(t *testing.T) {
	h := newHarness(t)
	h.expectError(h.do(http.MethodPatch, "/api/v1/messages/9999", map[string]any{"is_read": true}), 404, "not_found")
}

// --- bulk mark read -------------------------------------------------------

func TestMarkMessagesReadScopedToTheView(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(3)
	h.seedMessage(fixture.account.ID, fixture.archive.ID, 99, seedOptions{subject: "Archived"})

	response := h.do(http.MethodPost, "/api/v1/messages/mark-read?folder=inbox", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	result := decodeBody[struct {
		Updated int  `json:"updated"`
		Capped  bool `json:"capped"`
		Partial bool `json:"partial"`
	}](t, response)
	if result.Updated != 3 || result.Capped || result.Partial {
		t.Fatalf("result = %+v", result)
	}
	// The archived message was outside the view and is still unread.
	if got := h.listMessagePage("?folder=archive").UnreadTotal; got != 1 {
		t.Fatalf("archive unread = %d, want 1", got)
	}
	if got := h.listMessagePage("?folder=inbox").UnreadTotal; got != 0 {
		t.Fatalf("inbox unread = %d, want 0", got)
	}
}

func TestMarkMessagesReadOnAnEmptyViewIsANoop(t *testing.T) {
	h := newHarness(t)
	h.seedFeed(0)
	response := h.do(http.MethodPost, "/api/v1/messages/mark-read?folder=inbox", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if decodeBody[struct{ Updated int }](t, response).Updated != 0 {
		t.Fatal("updated should be 0")
	}
	h.provider.mu.Lock()
	bulk := len(h.provider.seenBulk)
	h.provider.mu.Unlock()
	if bulk != 0 {
		t.Fatal("an empty view still reached the provider")
	}
}

// A partial provider success still changed state, so it is reported as a success
// carrying partial rather than discarded as an error.
func TestMarkMessagesReadReportsAPartialSuccess(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(3)
	accepted := fixture.ids[:2]
	h.provider.set(func(f *fakeProvider) {
		f.seenAccept = accepted
		f.seenErr = ports.Unavailablef("one mailbox went away")
	})
	response := h.do(http.MethodPost, "/api/v1/messages/mark-read", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	result := decodeBody[struct {
		Updated int  `json:"updated"`
		Partial bool `json:"partial"`
	}](t, response)
	if result.Updated != 2 || !result.Partial {
		t.Fatalf("result = %+v", result)
	}
	// Only the accepted messages were written locally.
	if got := h.listMessagePage("").UnreadTotal; got != 1 {
		t.Fatalf("unread_total = %d, want 1", got)
	}
}

// A provider that accepted nothing is a real failure and must not report success.
func TestMarkMessagesReadFailsWhenNothingWasAccepted(t *testing.T) {
	h := newHarness(t)
	h.seedFeed(2)
	h.provider.set(func(f *fakeProvider) {
		f.seenAccept = []int64{}
		f.seenErr = ports.Unavailablef("account offline")
	})
	h.expectError(h.do(http.MethodPost, "/api/v1/messages/mark-read", nil), 503, "provider_unavailable")
	if got := h.listMessagePage("").UnreadTotal; got != 2 {
		t.Fatalf("unread_total = %d, want 2", got)
	}
}

func TestMarkMessagesReadRejectsBadFilters(t *testing.T) {
	h := newHarness(t)
	h.expectError(h.do(http.MethodPost, "/api/v1/messages/mark-read?account_id=abc", nil), 400, "invalid_filter")
	h.expectError(h.do(http.MethodPost, "/api/v1/messages/mark-read?mailbox_id=0", nil), 400, "invalid_filter")
}

// --- attachment download --------------------------------------------------

func TestDownloadAttachment(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(1)
	content := []byte("%PDF-1.4 fake report")
	blob, err := h.blobs.Put(context.Background(), strings.NewReader(string(content)), "cache")
	if err != nil {
		t.Fatal(err)
	}
	h.provider.set(func(f *fakeProvider) {
		f.attachBlob = blob
		f.attachMeta = domain.Attachment{ID: 7, MessageID: fixture.ids[0], Filename: "../../etc/report.pdf", ContentType: "application/pdf", SizeBytes: blob.SizeBytes}
	})
	response := h.do(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d/attachments/7", fixture.ids[0]), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if response.Body.String() != string(content) {
		t.Fatalf("body = %q", response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := response.Header().Get("Content-Length"); got != fmt.Sprint(blob.SizeBytes) {
		t.Fatalf("Content-Length = %q, want %d", got, blob.SizeBytes)
	}
	// The filename is reduced to its base, so a traversal in the MIME header
	// cannot steer where a client writes the file.
	disposition := response.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, `filename=report.pdf`) || strings.Contains(disposition, "..") {
		t.Fatalf("Content-Disposition = %q", disposition)
	}
}

// An attachment with no declared type must not be served as one the browser will
// sniff and execute.
func TestDownloadAttachmentDefaultsTheContentType(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(1)
	blob, err := h.blobs.Put(context.Background(), strings.NewReader("data"), "cache")
	if err != nil {
		t.Fatal(err)
	}
	h.provider.set(func(f *fakeProvider) {
		f.attachBlob = blob
		f.attachMeta = domain.Attachment{ID: 3, Filename: "note.txt"}
	})
	response := h.do(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d/attachments/3", fixture.ids[0]), nil)
	if got := response.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
}

func TestDownloadAttachmentNotFound(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(1)
	h.provider.set(func(f *fakeProvider) { f.attachErr = ports.NotFoundf("no such attachment") })
	h.expectError(h.do(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d/attachments/5", fixture.ids[0]), nil), 404, "not_found")
}

// A blob row whose file is gone must be an error, not an empty 200 the user saves
// as a corrupt document.
func TestDownloadAttachmentFailsWhenTheBlobIsMissing(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(1)
	h.provider.set(func(f *fakeProvider) {
		f.attachBlob = domain.BlobObject{ID: 1, StorageKey: "ab/cdmissing", SizeBytes: 10}
		f.attachMeta = domain.Attachment{ID: 1, Filename: "gone.bin"}
	})
	response := h.do(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d/attachments/1", fixture.ids[0]), nil)
	if response.Code == http.StatusOK {
		t.Fatalf("a missing blob returned 200 with %d bytes", response.Body.Len())
	}
}

func TestDownloadAttachmentRejectsBadIDs(t *testing.T) {
	h := newHarness(t)
	h.expectError(h.do(http.MethodGet, "/api/v1/messages/abc/attachments/1", nil), 400, "invalid_id")
	h.expectError(h.do(http.MethodGet, "/api/v1/messages/1/attachments/0", nil), 400, "invalid_id")
}

// newJSONRequest builds a request with a raw body, for the cases where the point
// is that the body is not what the handler expects.
func newJSONRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}
