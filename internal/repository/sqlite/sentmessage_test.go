//go:build sqlite_fts5

package sqlite

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"nexusmail/internal/domain"
)

// sentDraft stores a draft in the state the send worker hands to CreateSentMessage:
// delivered over SMTP, not yet linked to a stored message.
func sentDraft(t *testing.T, store *Store, accountID int64, messageID string) domain.Draft {
	t.Helper()
	now := time.Now().UnixMilli()
	draft := domain.Draft{
		AccountID: accountID, RFCMessageID: messageID, Revision: 1,
		ToJSON: `["them@example.com"]`, CCJSON: `[]`, BCCJSON: `[]`, Subject: "receipt",
		Status: "sending", RemoteSyncState: "dirty", CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateDraft(context.Background(), &draft); err != nil {
		t.Fatal(err)
	}
	return draft
}

// sentMessageFor builds the row the worker derives from a draft. The dedupe key is
// sha256 of the RFC message id, so the same draft always produces the same key.
func sentMessageFor(accountID int64, messageID string) domain.Message {
	now := time.Now().UnixMilli()
	digest := sha256.Sum256([]byte(messageID))
	rfc := messageID
	return domain.Message{
		AccountID: accountID, Direction: "outgoing", DedupeKey: digest[:], RFCMessageID: &rfc,
		Subject: "receipt", Sender: "me@example.com", Recipients: "them@example.com",
		FromJSON: "[]", ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]",
		BodyState: "ready", SentAt: &now, ReceivedAt: now, IsRead: true, CreatedAt: now, UpdatedAt: now,
	}
}

func draftSentMessageID(t *testing.T, store *Store, draftID int64) *int64 {
	t.Helper()
	var draft domain.Draft
	if err := store.db.WithContext(context.Background()).First(&draft, draftID).Error; err != nil {
		t.Fatalf("read draft %d: %v", draftID, err)
	}
	return draft.SentMessageID
}

func outgoingCount(t *testing.T, store *Store, accountID int64) int64 {
	t.Helper()
	var count int64
	if err := store.db.WithContext(context.Background()).Model(&domain.Message{}).
		Where("account_id = ? AND direction = 'outgoing'", accountID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

// The ordinary path: one delivery stores one outgoing message and links the draft to
// it, which is what lets the outbox show the sent copy rather than the draft.
func TestCreateSentMessageLinksTheDraft(t *testing.T) {
	store := openTestStore(t)
	account, _ := seedAccountMailbox(t, store)
	draft := sentDraft(t, store, account.ID, "<first@nexusmail.local>")

	message := sentMessageFor(account.ID, draft.RFCMessageID)
	if err := store.CreateSentMessage(context.Background(), &message, draft.ID); err != nil {
		t.Fatal(err)
	}
	if message.ID == 0 {
		t.Fatal("the stored message id was not written back, so the caller cannot reference it")
	}
	linked := draftSentMessageID(t, store, draft.ID)
	if linked == nil || *linked != message.ID {
		t.Fatalf("draft.sent_message_id = %v, want %d", linked, message.ID)
	}
}

// The branch that matters, and the one that had no test. SMTP delivery can succeed
// while the write that marks the draft 'sent' fails — the worker logs that drift
// rather than unsending — so a restart can hand the same draft back to the worker.
// The second delivery derives the same dedupe key from the same RFC message id, and
// the insert hits the unique index.
//
// Without the fallback that error propagates: the worker treats a delivered mail as
// a failed send, the outbox shows a failure for mail the recipient already has, and
// the draft never links to the copy that is sitting in the database. So the second
// call has to succeed, reuse the stored row rather than adding a second one, and
// still link the draft.
func TestCreateSentMessageIsIdempotentForOneDraft(t *testing.T) {
	store := openTestStore(t)
	account, _ := seedAccountMailbox(t, store)
	ctx := context.Background()
	draft := sentDraft(t, store, account.ID, "<retry@nexusmail.local>")

	first := sentMessageFor(account.ID, draft.RFCMessageID)
	if err := store.CreateSentMessage(ctx, &first, draft.ID); err != nil {
		t.Fatal(err)
	}

	second := sentMessageFor(account.ID, draft.RFCMessageID)
	if err := store.CreateSentMessage(ctx, &second, draft.ID); err != nil {
		t.Fatalf("a repeated delivery of one draft failed: %v — the worker would report a failure for mail already sent", err)
	}
	if second.ID != first.ID {
		t.Errorf("the second call reported id %d, want the stored %d", second.ID, first.ID)
	}
	if count := outgoingCount(t, store, account.ID); count != 1 {
		t.Errorf("%d outgoing messages stored for one draft, want 1: the user sees the sent mail twice", count)
	}
	linked := draftSentMessageID(t, store, draft.ID)
	if linked == nil || *linked != first.ID {
		t.Fatalf("draft.sent_message_id = %v after the repeat, want %d", linked, first.ID)
	}
}

// The dedupe key is scoped per account, so the same RFC message id under two accounts
// is two messages. Collapsing them would hide one account's sent mail behind
// another's, and the lookup in the fallback filters on account_id for that reason.
func TestCreateSentMessageKeepsAccountsApart(t *testing.T) {
	store := openTestStore(t)
	first, _ := seedAccountMailbox(t, store)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	other := domain.Account{
		Email: "second@example.com", DisplayName: "second", Provider: "qq", AuthType: "password",
		Username: "second@example.com", IMAPHost: "h", IMAPPort: 993, IMAPTLSMode: "implicit",
		SMTPHost: "h", SMTPPort: 465, SMTPTLSMode: "implicit", SecretCiphertext: []byte("k"),
		Status: "disconnected", CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateAccount(ctx, &other); err != nil {
		t.Fatal(err)
	}

	const shared = "<shared@nexusmail.local>"
	firstDraft := sentDraft(t, store, first.ID, shared)
	otherDraft := sentDraft(t, store, other.ID, shared)

	firstMessage := sentMessageFor(first.ID, shared)
	if err := store.CreateSentMessage(ctx, &firstMessage, firstDraft.ID); err != nil {
		t.Fatal(err)
	}
	otherMessage := sentMessageFor(other.ID, shared)
	if err := store.CreateSentMessage(ctx, &otherMessage, otherDraft.ID); err != nil {
		t.Fatalf("the same message id under a second account was refused: %v", err)
	}
	if otherMessage.ID == firstMessage.ID {
		t.Fatal("two accounts collapsed onto one sent message, so one account's sent mail is hidden behind the other's")
	}
	if count := outgoingCount(t, store, other.ID); count != 1 {
		t.Errorf("the second account has %d outgoing messages, want 1", count)
	}
}
