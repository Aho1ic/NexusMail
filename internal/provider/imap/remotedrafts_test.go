//go:build sqlite_fts5

package imap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"

	goimap "github.com/emersion/go-imap/v2"
)

// Remote drafts and attachment download are the two features that reach the
// provider on the user's behalf outside the sync loop. Both go through the
// foreground half of the command lock, and both are destructive on the server —
// SyncDraft appends and then expunges the copy it replaces — so what they do to
// the remote mailbox is asserted against the server, not against the local rows.

// draftsHarness stands up an account whose provider has a Drafts folder and waits
// for the supervisor to have adopted it.
func draftsHarness(t *testing.T) (*harness, context.CancelFunc) {
	t.Helper()
	h := newHarness(t)
	for _, name := range []string{"Drafts", "Sent"} {
		if err := h.user.Create(name, nil); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := h.supervisor.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); h.supervisor.Stop() })
	waitConnected(t, h)
	// The role is assigned during the mailbox refresh, so nothing may run until it
	// has landed; otherwise the failure looks like "drafts mailbox is unavailable".
	waitFor(t, 30*time.Second, func() bool {
		_, err := h.repo.GetMailboxByRole(context.Background(), h.account.ID, "drafts")
		return err == nil
	})
	return h, cancel
}

func addressJSON(values ...string) string {
	payload, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return string(payload)
}

// storeDraft writes a local draft row directly, which is the state the HTTP layer
// leaves behind before the supervisor is asked to mirror it.
func storeDraft(t *testing.T, h *harness, draft domain.Draft) domain.Draft {
	t.Helper()
	if draft.RFCMessageID == "" {
		draft.RFCMessageID = fmt.Sprintf("<local-%d@nexusmail.test>", time.Now().UnixNano())
	}
	if draft.ToJSON == "" {
		draft.ToJSON = addressJSON("her@example.com")
	}
	if draft.CCJSON == "" {
		draft.CCJSON = "[]"
	}
	if draft.BCCJSON == "" {
		draft.BCCJSON = "[]"
	}
	if draft.Status == "" {
		draft.Status = "draft"
	}
	if draft.RemoteSyncState == "" {
		draft.RemoteSyncState = "dirty"
	}
	draft.AccountID = h.account.ID
	draft.Revision = 1
	now := time.Now().UnixMilli()
	draft.CreatedAt, draft.UpdatedAt = now, now
	if err := h.repo.CreateDraft(context.Background(), &draft); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	return draft
}

// remoteMessages returns what a second client sees in a mailbox: the UID, the
// flags and the subject of every message the server still reports.
type remoteMessage struct {
	uid     goimap.UID
	subject string
	flags   []goimap.Flag
}

func remoteMessages(t *testing.T, h *harness, mailbox string) []remoteMessage {
	t.Helper()
	ctx := context.Background()
	client := h.connect(t, ctx)
	defer func() { _ = client.Logout().Wait() }()
	selected, err := client.Select(mailbox, nil).Wait()
	if err != nil {
		t.Fatalf("select %s: %v", mailbox, err)
	}
	if selected.NumMessages == 0 {
		return nil
	}
	sequence := goimap.SeqSet{}
	sequence.AddRange(1, selected.NumMessages)
	items, err := client.Fetch(sequence, &goimap.FetchOptions{UID: true, Flags: true, Envelope: true}).Collect()
	if err != nil {
		t.Fatalf("fetch %s: %v", mailbox, err)
	}
	result := make([]remoteMessage, 0, len(items))
	for _, item := range items {
		subject := ""
		if item.Envelope != nil {
			subject = item.Envelope.Subject
		}
		result = append(result, remoteMessage{uid: item.UID, subject: subject, flags: item.Flags})
	}
	return result
}

func TestSyncDraftAppendsToTheRemoteDraftsFolder(t *testing.T) {
	h, _ := draftsHarness(t)
	draft := storeDraft(t, h, domain.Draft{Subject: "季度报告", BodyText: "见附件", CCJSON: addressJSON("cc@example.com")})

	if err := h.supervisor.SyncDraft(context.Background(), draft.ID); err != nil {
		t.Fatalf("sync draft: %v", err)
	}

	remote := remoteMessages(t, h, "Drafts")
	if len(remote) != 1 {
		t.Fatalf("Drafts holds %d messages, want 1", len(remote))
	}
	if remote[0].subject != "季度报告" {
		t.Errorf("remote subject is %q, want 季度报告", remote[0].subject)
	}
	// A message in Drafts without \Draft is a message the provider's own client
	// will show as sent-but-not-really.
	if !hasFlag(remote[0].flags, goimap.FlagDraft) {
		t.Errorf("remote flags are %v, want to include \\Draft", remote[0].flags)
	}

	stored, _, err := h.repo.GetDraft(context.Background(), draft.ID)
	if err != nil {
		t.Fatalf("reload draft: %v", err)
	}
	if stored.RemoteSyncState != "synced" {
		t.Errorf("remote_sync_state is %q, want synced", stored.RemoteSyncState)
	}
	if stored.RemoteUID == nil || goimap.UID(*stored.RemoteUID) != remote[0].uid {
		t.Errorf("stored UID is %v, want %d", stored.RemoteUID, remote[0].uid)
	}
	if stored.RemoteMailboxID == nil {
		t.Fatal("stored draft has no remote mailbox")
	}
}

// The second sync is the one that can leave a duplicate behind: the provider has no
// update, so the new copy is appended and the old one has to be removed.
func TestSyncDraftReplacesTheCopyItSupersedes(t *testing.T) {
	h, _ := draftsHarness(t)
	draft := storeDraft(t, h, domain.Draft{Subject: "第一版", BodyText: "初稿"})
	ctx := context.Background()
	if err := h.supervisor.SyncDraft(ctx, draft.ID); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	first := remoteMessages(t, h, "Drafts")
	if len(first) != 1 {
		t.Fatalf("after the first sync Drafts holds %d, want 1", len(first))
	}

	stored, _, err := h.repo.GetDraft(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Subject = "第二版"
	stored.BodyText = "改过了"
	stored.RemoteSyncState = "dirty"
	if err := h.repo.UpdateDraft(ctx, &stored, stored.Revision); err != nil {
		t.Fatalf("update draft: %v", err)
	}
	if err := h.supervisor.SyncDraft(ctx, draft.ID); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	remote := remoteMessages(t, h, "Drafts")
	live := make([]remoteMessage, 0, len(remote))
	for _, message := range remote {
		if !hasFlag(message.flags, goimap.FlagDeleted) {
			live = append(live, message)
		}
	}
	if len(live) != 1 {
		t.Fatalf("Drafts holds %d live messages after the second sync, want 1: %+v", len(live), remote)
	}
	if live[0].subject != "第二版" {
		t.Errorf("surviving subject is %q, want 第二版", live[0].subject)
	}
	if live[0].uid == first[0].uid {
		t.Error("the second sync reused the first UID; the old copy was never replaced")
	}
}

func TestSyncDraftCarriesAttachments(t *testing.T) {
	h, _ := draftsHarness(t)
	ctx := context.Background()
	draft := storeDraft(t, h, domain.Draft{Subject: "带附件", BodyText: "见附件"})

	payload := []byte("attachment bytes\x00\xff")
	// Draft attachments are "durable": an unknown send result must not be able to
	// lose the bytes the user attached.
	blob, err := h.supervisor.blobs.Put(ctx, strings.NewReader(string(payload)), "durable")
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	if err := h.repo.AddDraftAttachment(ctx, &domain.DraftAttachment{
		DraftID: draft.ID, BlobID: blob.ID, Filename: "report.bin",
		ContentType: "application/octet-stream", SizeBytes: int64(len(payload)), CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("add attachment: %v", err)
	}

	if err := h.supervisor.SyncDraft(ctx, draft.ID); err != nil {
		t.Fatalf("sync draft: %v", err)
	}

	raw := remoteBody(t, h, "Drafts")
	if !strings.Contains(raw, "report.bin") {
		t.Error("the appended draft does not name the attachment")
	}
	if !strings.Contains(raw, "multipart/mixed") {
		t.Error("the appended draft is not multipart, so the attachment cannot be in it")
	}
}

// remoteBody returns the full source of the single message in a mailbox.
func remoteBody(t *testing.T, h *harness, mailbox string) string {
	t.Helper()
	ctx := context.Background()
	client := h.connect(t, ctx)
	defer func() { _ = client.Logout().Wait() }()
	selected, err := client.Select(mailbox, nil).Wait()
	if err != nil {
		t.Fatalf("select %s: %v", mailbox, err)
	}
	if selected.NumMessages == 0 {
		t.Fatalf("%s is empty", mailbox)
	}
	section := &goimap.FetchItemBodySection{}
	sequence := goimap.SeqSet{}
	sequence.AddNum(selected.NumMessages)
	items, err := client.Fetch(sequence, &goimap.FetchOptions{BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
	if err != nil {
		t.Fatalf("fetch body: %v", err)
	}
	return string(items[0].FindBodySection(section))
}

// Only a draft the user can still edit may be mirrored. A queued or sending draft
// belongs to the send worker, and appending it to Drafts would leave the provider
// showing a copy of mail that is already on its way.
func TestSyncDraftIgnoresDraftsTheWorkerOwns(t *testing.T) {
	h, _ := draftsHarness(t)
	ctx := context.Background()
	for _, status := range []string{"queued", "sending", "sent", "retry_wait"} {
		draft := storeDraft(t, h, domain.Draft{Subject: "状态 " + status, Status: status})
		if err := h.supervisor.SyncDraft(ctx, draft.ID); err != nil {
			t.Fatalf("sync %s: %v", status, err)
		}
	}
	if remote := remoteMessages(t, h, "Drafts"); len(remote) != 0 {
		t.Fatalf("Drafts holds %d messages, want 0: %+v", len(remote), remote)
	}

	// The three the user still owns do get mirrored.
	for _, status := range []string{"draft", "failed", "unknown"} {
		draft := storeDraft(t, h, domain.Draft{Subject: "可编辑 " + status, Status: status})
		if err := h.supervisor.SyncDraft(ctx, draft.ID); err != nil {
			t.Fatalf("sync %s: %v", status, err)
		}
	}
	if remote := remoteMessages(t, h, "Drafts"); len(remote) != 3 {
		t.Fatalf("Drafts holds %d messages, want 3", len(remote))
	}
}

func TestSyncDraftReportsAnUnusableAddressList(t *testing.T) {
	h, _ := draftsHarness(t)
	ctx := context.Background()
	draft := storeDraft(t, h, domain.Draft{Subject: "坏地址", ToJSON: addressJSON("not an address")})

	if err := h.supervisor.SyncDraft(ctx, draft.ID); err == nil {
		t.Fatal("sync accepted an unparseable recipient")
	}
	// Nothing may reach the provider when the payload could not be composed.
	if remote := remoteMessages(t, h, "Drafts"); len(remote) != 0 {
		t.Fatalf("Drafts holds %d messages, want 0", len(remote))
	}
}

func TestSyncDraftReportsAMissingDraftsMailbox(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()
	waitConnected(t, h)

	draft := storeDraft(t, h, domain.Draft{Subject: "无处可放"})
	err := h.supervisor.SyncDraft(ctx, draft.ID)
	if err == nil {
		t.Fatal("sync succeeded without a remote Drafts folder")
	}
	// Unavailable, not Invalid: the draft is fine, the provider has nowhere to put it.
	if !errors.Is(err, ports.ErrUnavailable) {
		t.Errorf("error is %v, want an unavailable error", err)
	}
}

func TestSyncDraftReportsAnUnknownDraft(t *testing.T) {
	h, _ := draftsHarness(t)
	if err := h.supervisor.SyncDraft(context.Background(), 999999); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("error is %v, want not found", err)
	}
}

func TestDeleteRemoteDraftRemovesTheCopyItOwns(t *testing.T) {
	h, _ := draftsHarness(t)
	ctx := context.Background()
	draft := storeDraft(t, h, domain.Draft{Subject: "要删除"})
	if err := h.supervisor.SyncDraft(ctx, draft.ID); err != nil {
		t.Fatalf("sync draft: %v", err)
	}
	if remote := remoteMessages(t, h, "Drafts"); len(remote) != 1 {
		t.Fatalf("setup left %d messages in Drafts, want 1", len(remote))
	}

	if err := h.supervisor.DeleteRemoteDraft(ctx, draft.ID); err != nil {
		t.Fatalf("delete remote draft: %v", err)
	}

	for _, message := range remoteMessages(t, h, "Drafts") {
		if !hasFlag(message.flags, goimap.FlagDeleted) {
			t.Errorf("UID %d survived without \\Deleted: %v", message.uid, message.flags)
		}
	}
}

// A draft that was never mirrored has nothing to remove, and asking the provider
// about a UID it never issued is how a delete turns into an error on a send that
// otherwise succeeded.
func TestDeleteRemoteDraftIsANoOpWithoutARemoteCopy(t *testing.T) {
	h, _ := draftsHarness(t)
	draft := storeDraft(t, h, domain.Draft{Subject: "从未同步"})
	if err := h.supervisor.DeleteRemoteDraft(context.Background(), draft.ID); err != nil {
		t.Fatalf("delete without a remote copy: %v", err)
	}
}

// UIDVALIDITY changing means every UID the provider once issued now refers to
// something else, so acting on a stored UID would delete a stranger's mail.
func TestDeleteRemoteDraftStopsWhenUIDValidityMoved(t *testing.T) {
	h, _ := draftsHarness(t)
	ctx := context.Background()
	draft := storeDraft(t, h, domain.Draft{Subject: "旧世代"})
	if err := h.supervisor.SyncDraft(ctx, draft.ID); err != nil {
		t.Fatalf("sync draft: %v", err)
	}

	stored, _, err := h.repo.GetDraft(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "drafts")
	if err != nil {
		t.Fatal(err)
	}
	stale := stored.RemoteUIDValidity
	if stale == nil {
		t.Fatal("the synced draft carries no UIDVALIDITY")
	}
	bogus := *stale + 1
	if err := h.repo.UpdateDraftRemote(ctx, draft.ID, mailbox.ID, bogus, *stored.RemoteUID, time.Now().UnixMilli(), "synced", nil); err != nil {
		t.Fatalf("rewrite validity: %v", err)
	}

	if err := h.supervisor.DeleteRemoteDraft(ctx, draft.ID); err != nil {
		t.Fatalf("delete with a stale validity: %v", err)
	}
	// The message must still be there. Asserting only that nothing carries \Deleted
	// would pass vacuously against a provider with UIDPLUS, where the expunge that
	// follows the store removes the message outright and leaves nothing to inspect.
	survivors := remoteMessages(t, h, "Drafts")
	if len(survivors) != 1 {
		t.Fatalf("Drafts holds %d messages after a stale-validity delete, want the untouched 1", len(survivors))
	}
	if survivors[0].subject != "旧世代" || hasFlag(survivors[0].flags, goimap.FlagDeleted) {
		t.Errorf("UID %d was touched despite a UIDVALIDITY change: %+v", survivors[0].uid, survivors[0])
	}
}

func TestAppendSentReportsAMissingSentMailbox(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()
	waitConnected(t, h)

	if err := h.supervisor.AppendSent(ctx, h.account.ID, []byte(rawMessage("nowhere"))); !errors.Is(err, ports.ErrUnavailable) {
		t.Errorf("error is %v, want an unavailable error", err)
	}
}

func TestAppendSentMarksTheCopySeen(t *testing.T) {
	h, _ := draftsHarness(t)
	payload := []byte(rawMessage("outgoing"))
	if err := h.supervisor.AppendSent(context.Background(), h.account.ID, payload); err != nil {
		t.Fatalf("append sent: %v", err)
	}

	remote := remoteMessages(t, h, "Sent")
	if len(remote) != 1 {
		t.Fatalf("Sent holds %d messages, want 1", len(remote))
	}
	// Mail the user sent is not unread mail; without \Seen every provider client
	// shows the Sent folder with a permanent unread badge.
	if !hasFlag(remote[0].flags, goimap.FlagSeen) {
		t.Errorf("flags are %v, want to include \\Seen", remote[0].flags)
	}
}

func TestRemoteDraftsAreAdoptedFromTheProvider(t *testing.T) {
	h, _ := draftsHarness(t)
	ctx := context.Background()
	raw := "MIME-Version: 1.0\r\nMessage-Id: <remote-draft@example.com>\r\n" +
		"From: mail@example.com\r\nTo: her@example.com\r\nCc: cc@example.com\r\n" +
		"Subject: 服务商草稿\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n在别处写的\r\n"
	if _, err := h.user.Append("Drafts", literal{strings.NewReader(raw)}, &goimap.AppendOptions{Flags: []goimap.Flag{goimap.FlagDraft}, Time: time.Now()}); err != nil {
		t.Fatalf("append remote draft: %v", err)
	}
	drafts, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "drafts")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.supervisor.RequestMailbox(ctx, drafts.ID); err != nil {
		t.Fatalf("request the drafts mailbox: %v", err)
	}

	var adopted domain.Draft
	waitFor(t, 60*time.Second, func() bool {
		drafts, err := h.repo.ListDrafts(ctx, "")
		if err != nil {
			return false
		}
		for _, item := range drafts {
			if item.Subject == "服务商草稿" {
				adopted = item
				return true
			}
		}
		return false
	})

	// A draft written in another client has to arrive complete enough to edit here.
	if adopted.RemoteSyncState != "synced" {
		t.Errorf("remote_sync_state is %q, want synced", adopted.RemoteSyncState)
	}
	if adopted.RemoteUID == nil || adopted.RemoteMailboxID == nil {
		t.Error("the adopted draft carries no remote location")
	}
	if !strings.Contains(adopted.ToJSON, "her@example.com") {
		t.Errorf("recipients are %q, want to include her@example.com", adopted.ToJSON)
	}
	if !strings.Contains(adopted.CCJSON, "cc@example.com") {
		t.Errorf("cc is %q, want to include cc@example.com", adopted.CCJSON)
	}
	if !strings.Contains(adopted.BodyText, "在别处写的") {
		t.Errorf("body is %q, want the remote text", adopted.BodyText)
	}
}

// A draft too large to import inline is stored from the IMAP envelope alone,
// which is the only path through imapAddresses and encodeMailAddresses. It must
// still arrive with its recipients: a draft that opens with an empty To field
// silently drops who it was addressed to.
func TestRemoteDraftsTooLargeToInlineKeepTheirEnvelope(t *testing.T) {
	h, _ := draftsHarness(t)
	ctx := context.Background()
	filler := strings.Repeat("padding line to push the draft past the inline import ceiling\r\n", 20000)
	raw := "MIME-Version: 1.0\r\nMessage-Id: <oversized-draft@example.com>\r\n" +
		"From: mail@example.com\r\nTo: Her Name <her@example.com>\r\nCc: cc@example.com\r\n" +
		"Subject: 超大草稿\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + filler
	if int64(len(raw)) <= maxInlineDraftImportBytes {
		t.Fatalf("the fixture is %d bytes, which is still inlined", len(raw))
	}
	if _, err := h.user.Append("Drafts", literal{strings.NewReader(raw)}, &goimap.AppendOptions{Flags: []goimap.Flag{goimap.FlagDraft}, Time: time.Now()}); err != nil {
		t.Fatalf("append oversized draft: %v", err)
	}
	drafts, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "drafts")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.supervisor.RequestMailbox(ctx, drafts.ID); err != nil {
		t.Fatalf("request the drafts mailbox: %v", err)
	}

	var adopted domain.Draft
	waitFor(t, 60*time.Second, func() bool {
		items, err := h.repo.ListDrafts(ctx, "")
		if err != nil {
			return false
		}
		for _, item := range items {
			if item.Subject == "超大草稿" {
				adopted = item
				return true
			}
		}
		return false
	})

	// The display name has to survive the envelope round trip, not just the address.
	if !strings.Contains(adopted.ToJSON, "her@example.com") || !strings.Contains(adopted.ToJSON, "Her Name") {
		t.Errorf("recipients are %q, want Her Name <her@example.com>", adopted.ToJSON)
	}
	if !strings.Contains(adopted.CCJSON, "cc@example.com") {
		t.Errorf("cc is %q, want cc@example.com", adopted.CCJSON)
	}
	// No body was downloaded, so there is nothing to show — but the draft still has
	// to be editable rather than absent.
	if adopted.RemoteUID == nil {
		t.Error("the oversized draft carries no remote UID")
	}
}

// A draft the provider stores without a Message-Id still needs a local identity,
// and it has to be derived from where the draft lives rather than from the clock:
// rfc_message_id is unique per account and is the id the message would be sent
// under, so a value that changes every pass makes the same remote draft look like
// a different one each time it is seen.
func TestRemoteDraftsWithoutAMessageIDAreAdoptedOnce(t *testing.T) {
	h, _ := draftsHarness(t)
	ctx := context.Background()
	raw := "MIME-Version: 1.0\r\nFrom: mail@example.com\r\nTo: her@example.com\r\n" +
		"Subject: 无编号草稿\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n没有 Message-Id\r\n"
	if _, err := h.user.Append("Drafts", literal{strings.NewReader(raw)}, &goimap.AppendOptions{Flags: []goimap.Flag{goimap.FlagDraft}, Time: time.Now()}); err != nil {
		t.Fatalf("append: %v", err)
	}
	drafts, err := h.repo.GetMailboxByRole(ctx, h.account.ID, "drafts")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.supervisor.RequestMailbox(ctx, drafts.ID); err != nil {
		t.Fatal(err)
	}

	count := func() int {
		items, err := h.repo.ListDrafts(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, item := range items {
			if item.Subject == "无编号草稿" {
				n++
			}
		}
		return n
	}
	waitFor(t, 60*time.Second, func() bool { return count() == 1 })

	// The id has to name the account, mailbox, UIDVALIDITY and UID it stands for.
	// Nothing else about the draft is stable, so anything derived from the clock or
	// a counter would give the same remote draft a new identity on every pass — and
	// ReconcileRemoteDraft's UID fallback would hide that here while a fresh import
	// or a second instance produced a duplicate.
	var adopted domain.Draft
	items, err := h.repo.ListDrafts(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Subject == "无编号草稿" {
			adopted = item
		}
	}
	if adopted.RemoteUID == nil || adopted.RemoteUIDValidity == nil {
		t.Fatal("the adopted draft carries no remote location")
	}
	want := fmt.Sprintf("<remote-%d-%d-%d-%d@nexusmail.local>", h.account.ID, drafts.ID, *adopted.RemoteUIDValidity, *adopted.RemoteUID)
	if adopted.RFCMessageID != want {
		t.Errorf("synthesised id is %q, want %q", adopted.RFCMessageID, want)
	}

	// And a second pass over the same mailbox must not add a row.
	time.Sleep(requestSyncCooldown)
	if err := h.supervisor.RequestMailbox(ctx, drafts.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if n := count(); n != 1 {
		t.Errorf("the draft was adopted %d times, want 1", n)
	}
}

// pollWithoutIdle is the fallback for a server that never offers IDLE. go-imap
// advertises IDLE for both IMAP4rev1 and IMAP4rev2, so no in-memory server can
// reach this loop through the idle path; it is driven directly instead.
func TestPollWithoutIdleSignalsOnEveryTick(t *testing.T) {
	h := newHarness(t)
	rt := &runtime{account: h.account, syncReq: make(chan int64, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan bool, 1)
	go func() { done <- h.supervisor.pollWithoutIdle(ctx, rt, 20*time.Millisecond, time.Hour) }()

	// Two ticks, drained between them: the loop has to keep signalling rather than
	// firing once and settling.
	for round := range 2 {
		select {
		case <-rt.syncReq:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: no sync request from the poll loop", round)
		}
	}

	cancel()
	select {
	case again := <-done:
		if again {
			t.Error("a cancelled poll loop asked to re-probe capabilities instead of exiting")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poll loop ignored the cancelled context")
	}
}

// After the recheck interval the loop returns so the idle loop reconnects and asks
// for capabilities again — a provider that gains IDLE must not be polled forever.
func TestPollWithoutIdleRechecksCapabilities(t *testing.T) {
	h := newHarness(t)
	rt := &runtime{account: h.account, syncReq: make(chan int64, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	again := h.supervisor.pollWithoutIdle(ctx, rt, time.Hour, 50*time.Millisecond)
	if !again {
		t.Error("the poll loop exited instead of asking to re-probe capabilities")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("the loop returned after %s, before the recheck interval elapsed", elapsed)
	}
}

func TestFetchAttachmentPullsTheBytesOnDemand(t *testing.T) {
	h, _ := draftsHarness(t)
	ctx := context.Background()
	const content = "attachment payload"
	raw := "MIME-Version: 1.0\r\nMessage-Id: <with-attachment@example.com>\r\n" +
		"From: Sender <sender@example.com>\r\nTo: mail@example.com\r\nSubject: 附件邮件\r\n" +
		"Content-Type: multipart/mixed; boundary=BOUND\r\n\r\n" +
		"--BOUND\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody text\r\n" +
		"--BOUND\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"payload.bin\"\r\n\r\n" + content + "\r\n" +
		"--BOUND--\r\n"
	if _, err := h.user.Append("INBOX", literal{strings.NewReader(raw)}, &goimap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatalf("append: %v", err)
	}

	messageID := waitForMessage(t, h)
	var attachment domain.Attachment
	waitFor(t, 60*time.Second, func() bool {
		_, attachments, err := h.repo.GetMessage(ctx, messageID)
		if err != nil || len(attachments) == 0 {
			return false
		}
		attachment = attachments[0]
		return true
	})
	if attachment.Filename != "payload.bin" {
		t.Fatalf("attachment filename is %q, want payload.bin", attachment.Filename)
	}

	blob, fetched, err := h.supervisor.FetchAttachment(ctx, messageID, attachment.ID)
	if err != nil {
		t.Fatalf("fetch attachment: %v", err)
	}
	if fetched.FetchState != "ready" {
		t.Errorf("fetch_state is %q, want ready", fetched.FetchState)
	}
	reader, err := h.supervisor.blobs.Open(ctx, blob)
	if err != nil {
		t.Fatalf("open blob: %v", err)
	}
	defer func() { _ = reader.Close() }()
	stored, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	// Exactly that part, not the message it came from. `Contains` alone would accept
	// a fetch of the whole message, since the attachment bytes are inside it — and
	// what the browser would then download is the raw MIME source.
	if got := strings.TrimSpace(string(stored)); got != content {
		t.Errorf("stored bytes are %q, want exactly %q", got, content)
	}
	for _, foreign := range []string{"body text", "Subject:", "BOUND"} {
		if strings.Contains(string(stored), foreign) {
			t.Errorf("the blob contains %q, so it holds more than the attachment part", foreign)
		}
	}

	// The row now points at the blob, which is what lets the next request skip the
	// provider entirely.
	_, stored2, err := h.repo.GetMessage(ctx, messageID)
	if err != nil {
		t.Fatal(err)
	}
	if stored2[0].BlobID == nil || *stored2[0].BlobID != blob.ID {
		t.Errorf("the attachment row points at %v, want blob %d", stored2[0].BlobID, blob.ID)
	}

	// Comparing blob IDs cannot show a cache hit: CreateBlob dedupes by sha256 and
	// hands back the same row, so re-downloading the same bytes is indistinguishable
	// from not downloading them. Taking the provider away is what separates the two —
	// after Stop the connection is dead, so only a served-from-disk read can succeed.
	h.supervisor.Stop()
	again, cached, err := h.supervisor.FetchAttachment(ctx, messageID, attachment.ID)
	if err != nil {
		t.Fatalf("a cached attachment could not be read with the provider gone: %v", err)
	}
	if again.ID != blob.ID {
		t.Errorf("second fetch returned blob %d, want the cached %d", again.ID, blob.ID)
	}
	if cached.BlobID == nil {
		t.Error("the cached attachment came back without its blob")
	}
}

func TestFetchAttachmentReportsAnUnknownAttachment(t *testing.T) {
	h, _ := draftsHarness(t)
	h.deliver(t, "plain")
	messageID := waitForMessage(t, h)
	if _, _, err := h.supervisor.FetchAttachment(context.Background(), messageID, 999999); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("error is %v, want not found", err)
	}
}

func TestParsePartIDRejectsWhatCannotBeAPart(t *testing.T) {
	for _, value := range []string{"", "0", "-1", "1.0", "abc", "1.b", "1..2", " 1"} {
		if _, err := parsePartID(value); !errors.Is(err, ports.ErrInvalidInput) {
			t.Errorf("parsePartID(%q) returned %v, want an invalid error", value, err)
		}
	}
	for value, want := range map[string][]int{"1": {1}, "2.1": {2, 1}, "3.2.1": {3, 2, 1}} {
		got, err := parsePartID(value)
		if err != nil {
			t.Fatalf("parsePartID(%q): %v", value, err)
		}
		if len(got) != len(want) {
			t.Fatalf("parsePartID(%q) = %v, want %v", value, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parsePartID(%q) = %v, want %v", value, got, want)
			}
		}
	}
}
