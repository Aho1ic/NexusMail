package imap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"nexusmail/internal/domain"
	mailparser "nexusmail/internal/mail"
	"nexusmail/internal/ports"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Remote draft synchronisation and the Sent APPEND.

func (s *Supervisor) SyncDraft(ctx context.Context, draftID int64) error {
	draft, attachments, err := s.repo.GetDraft(ctx, draftID)
	if err != nil {
		return err
	}
	if draft.Status != "draft" && draft.Status != "failed" && draft.Status != "unknown" {
		return nil
	}
	account, err := s.repo.GetAccount(ctx, draft.AccountID)
	if err != nil {
		return err
	}
	mailbox, err := s.repo.GetMailboxByRole(ctx, draft.AccountID, "drafts")
	if err != nil {
		return ports.Unavailablef("remote drafts mailbox is unavailable")
	}
	to, err := decodeDraftAddresses(draft.ToJSON)
	if err != nil {
		return err
	}
	cc, err := decodeDraftAddresses(draft.CCJSON)
	if err != nil {
		return err
	}
	bcc, err := decodeDraftAddresses(draft.BCCJSON)
	if err != nil {
		return err
	}
	outgoingAttachments := make([]mailparser.OutgoingAttachment, 0, len(attachments))
	var closers []interface{ Close() error }
	defer func() {
		for _, closer := range closers {
			_ = closer.Close()
		}
	}()
	for _, attachment := range attachments {
		blob, err := s.repo.GetBlob(ctx, attachment.BlobID)
		if err != nil {
			return err
		}
		reader, err := s.blobs.Open(ctx, blob)
		if err != nil {
			return err
		}
		closers = append(closers, reader)
		outgoingAttachments = append(outgoingAttachments, mailparser.OutgoingAttachment{Filename: attachment.Filename, ContentType: attachment.ContentType, Data: reader})
	}
	payload, err := mailparser.Compose(mailparser.Outgoing{
		MessageID: draft.RFCMessageID, From: mail.Address{Name: account.DisplayName, Address: account.Email},
		To: to, CC: cc, BCC: bcc, Subject: draft.Subject, BodyText: draft.BodyText, Attachments: outgoingAttachments,
	})
	if err != nil {
		return err
	}
	rt, err := s.runtime(account.ID)
	if err != nil {
		return err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return ports.Unavailablef("account is offline")
	}
	command := client.Append(mailbox.RemoteName, int64(len(payload)), &goimap.AppendOptions{Flags: []goimap.Flag{goimap.FlagDraft}, Time: time.Now()})
	if _, err := command.Write(payload); err != nil {
		_ = command.Close()
		return err
	}
	if err := command.Close(); err != nil {
		return err
	}
	appended, err := command.Wait()
	if err != nil {
		return err
	}
	if draft.RemoteUID != nil && draft.RemoteUIDValidity != nil && *draft.RemoteUIDValidity == appended.UIDValidity {
		if _, selectErr := client.Select(mailbox.RemoteName, nil).Wait(); selectErr == nil {
			old := goimap.UIDSetNum(goimap.UID(*draft.RemoteUID))
			if _, storeErr := client.Store(old, &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Collect(); storeErr == nil && client.Caps().Has(goimap.CapUIDPlus) {
				_, _ = client.UIDExpunge(old).Collect()
			}
		}
	}
	now := time.Now().UnixMilli()
	return s.repo.UpdateDraftRemote(ctx, draft.ID, mailbox.ID, appended.UIDValidity, uint32(appended.UID), now, "synced", nil)
}

func (s *Supervisor) DeleteRemoteDraft(ctx context.Context, draftID int64) error {
	draft, _, err := s.repo.GetDraft(ctx, draftID)
	if err != nil {
		return err
	}
	if draft.RemoteMailboxID == nil || draft.RemoteUID == nil {
		return nil
	}
	mailbox, err := s.repo.GetMailboxByRole(ctx, draft.AccountID, "drafts")
	if err != nil {
		return err
	}
	rt, err := s.runtime(draft.AccountID)
	if err != nil {
		return err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return ports.Unavailablef("account is offline")
	}
	selected, err := client.Select(mailbox.RemoteName, nil).Wait()
	if err != nil {
		return err
	}
	if draft.RemoteUIDValidity != nil && selected.UIDValidity != *draft.RemoteUIDValidity {
		return nil
	}
	uids := goimap.UIDSetNum(goimap.UID(*draft.RemoteUID))
	if _, err := client.Store(uids, &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Collect(); err != nil {
		return err
	}
	if client.Caps().Has(goimap.CapUIDPlus) {
		_, err = client.UIDExpunge(uids).Collect()
		return err
	}
	return nil
}

func (s *Supervisor) storeRemoteDraft(ctx context.Context, mailbox domain.Mailbox, fetched *imapclient.FetchMessageBuffer, raw []byte) (bool, int64, error) {
	if fetched.Envelope == nil {
		return false, 0, nil
	}
	parsed := mailparser.Parsed{}
	if len(raw) > 0 {
		value, err := mailparser.Parse(bytes.NewReader(raw))
		if err != nil {
			return false, 0, err
		}
		parsed = value
	}
	rfcID := strings.TrimSpace(fetched.Envelope.MessageID)
	if rfcID == "" {
		rfcID = fmt.Sprintf("<remote-%d-%d-%d-%d@nexusmail.local>", mailbox.AccountID, mailbox.ID, mailbox.UIDValidity, fetched.UID)
	}
	to := parsed.To
	cc := parsed.CC
	bcc := parsed.BCC
	if len(to) == 0 {
		to = imapAddresses(fetched.Envelope.To)
	}
	if len(cc) == 0 {
		cc = imapAddresses(fetched.Envelope.Cc)
	}
	remoteTime := fetched.InternalDate.UnixMilli()
	if remoteTime <= 0 {
		remoteTime = time.Now().UnixMilli()
	}
	mailboxID, validity, uid := mailbox.ID, mailbox.UIDValidity, uint32(fetched.UID)
	draft := domain.Draft{
		AccountID: mailbox.AccountID, RFCMessageID: rfcID, Revision: 1,
		ToJSON: encodeMailAddresses(to), CCJSON: encodeMailAddresses(cc), BCCJSON: encodeMailAddresses(bcc),
		Subject: fetched.Envelope.Subject, BodyText: parsed.Text, Status: "draft", RemoteSyncState: "synced",
		RemoteMailboxID: &mailboxID, RemoteUIDValidity: &validity, RemoteUID: &uid, RemoteUpdatedAt: &remoteTime,
		CreatedAt: remoteTime, UpdatedAt: remoteTime,
	}
	if parsed.Subject != "" {
		draft.Subject = parsed.Subject
	}
	reconciled, changed, err := s.repo.ReconcileRemoteDraft(ctx, &draft)
	return changed, reconciled.ID, err
}

func (s *Supervisor) AppendSent(ctx context.Context, accountID int64, payload []byte) error {
	mailbox, err := s.repo.GetMailboxByRole(ctx, accountID, "sent")
	if err != nil {
		return ports.Unavailablef("remote sent mailbox is unavailable")
	}
	rt, err := s.runtime(accountID)
	if err != nil {
		return err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return ports.Unavailablef("account is offline")
	}
	command := client.Append(mailbox.RemoteName, int64(len(payload)), &goimap.AppendOptions{Flags: []goimap.Flag{goimap.FlagSeen}, Time: time.Now()})
	if _, err := command.Write(payload); err != nil {
		_ = command.Close()
		return err
	}
	if err := command.Close(); err != nil {
		return err
	}
	_, err = command.Wait()
	return err
}

func decodeDraftAddresses(raw string) ([]mail.Address, error) {
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, err
	}
	result := make([]mail.Address, 0, len(values))
	for _, value := range values {
		address, err := mail.ParseAddress(value)
		if err != nil {
			return nil, err
		}
		result = append(result, *address)
	}
	return result, nil
}
