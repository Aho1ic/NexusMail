package imap

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"nexusmail/internal/domain"
	mailparser "nexusmail/internal/mail"
	"nexusmail/internal/ports"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Turning a fetched IMAP message into rows: header decoding, address formatting,
// and the batched write.

// pendingMessage is one row ready to be flushed by the supervisor at the end
// of a UID chunk. input is the message + mailbox mapping; attachments are
// kept here so the batch can persist them with the same writeMu section.
type pendingMessage struct {
	input       ports.MessageInput
	attachments []domain.Attachment
	fetched     *imapclient.FetchMessageBuffer
	uid         uint32
}

// buildFetchedMessage turns an IMAP FetchMessageBuffer into the shape the
// repository ingests. It does no I/O: the supervisor collects a chunk and
// flushes it under one transaction.
func (s *Supervisor) buildFetchedMessage(mailbox domain.Mailbox, fetched *imapclient.FetchMessageBuffer) (ports.MessageInput, []domain.Attachment, error) {
	if fetched.Envelope == nil {
		return ports.MessageInput{}, nil, errors.New("fetched message has no envelope")
	}
	envelope := fetched.Envelope
	rfcID := strings.TrimSpace(envelope.MessageID)
	dedupeSource := rfcID + "\x00" + strconv.FormatInt(fetched.RFC822Size, 10)
	if rfcID == "" {
		dedupeSource = fmt.Sprintf("%d:%d:%d:%d", mailbox.ID, mailbox.UIDValidity, fetched.UID, fetched.RFC822Size)
	}
	digest := sha256.Sum256([]byte(dedupeSource))
	from := addresses(envelope.From)
	to := addresses(envelope.To)
	cc := addresses(envelope.Cc)
	fromJSON, _ := json.Marshal(from)
	toJSON, _ := json.Marshal(to)
	ccJSON, _ := json.Marshal(cc)
	now := time.Now().UnixMilli()
	received := fetched.InternalDate.UnixMilli()
	if received == 0 {
		received = now
	}
	message := domain.Message{
		AccountID: mailbox.AccountID, Direction: "incoming", DedupeKey: digest[:], Subject: envelope.Subject,
		Sender: strings.Join(from, " "), Recipients: strings.Join(append(to, cc...), " "), FromJSON: string(fromJSON), ToJSON: string(toJSON), CCJSON: string(ccJSON),
		BCCJSON: "[]", ReplyToJSON: "[]", ReferencesJSON: "[]", BodyState: "metadata", SizeBytes: fetched.RFC822Size,
		ReceivedAt: received, IsRead: hasFlag(fetched.Flags, goimap.FlagSeen), IsStarred: hasFlag(fetched.Flags, goimap.FlagFlagged), CreatedAt: now, UpdatedAt: now,
	}
	if rfcID != "" {
		message.RFCMessageID = &rfcID
	}
	if !envelope.Date.IsZero() {
		value := envelope.Date.UnixMilli()
		message.SentAt = &value
	}
	flagValues := make([]string, len(fetched.Flags))
	for i, flag := range fetched.Flags {
		flagValues[i] = string(flag)
	}
	var attachments []domain.Attachment
	if fetched.BodyStructure != nil {
		fetched.BodyStructure.Walk(func(path []int, part goimap.BodyStructure) bool {
			single, ok := part.(*goimap.BodyStructureSinglePart)
			if !ok {
				return true
			}
			filename := single.Filename()
			disposition := single.Disposition()
			if filename == "" && disposition == nil {
				return true
			}
			dispositionValue := "attachment"
			if disposition != nil && strings.EqualFold(disposition.Value, "inline") {
				dispositionValue = "inline"
			}
			partValues := make([]string, len(path))
			for i, value := range path {
				partValues[i] = strconv.Itoa(value)
			}
			att := domain.Attachment{PartID: strings.Join(partValues, "."), Filename: filename, ContentType: single.MediaType(), Disposition: dispositionValue, SizeBytes: int64(single.Size), FetchState: "metadata", CreatedAt: now, UpdatedAt: now}
			if single.ID != "" {
				value := single.ID
				att.ContentID = &value
			}
			attachments = append(attachments, att)
			return true
		})
	}
	return ports.MessageInput{
		Message:      &message,
		MailboxID:    mailbox.ID,
		UID:          uint32(fetched.UID),
		Flags:        flagValues,
		InternalDate: fetched.InternalDate,
	}, attachments, nil
}

// flushPending commits a chunk of built messages under a single writeMu and
// publishes a NEW_EMAIL event for each newly created row. Attachments are
// written in the same single-transaction section; MessageID on each
// attachment is patched to the id the repository assigned.
func (s *Supervisor) flushPending(ctx context.Context, mailbox domain.Mailbox, pending []pendingMessage) error {
	inputs := make([]ports.MessageInput, len(pending))
	for i, item := range pending {
		inputs[i] = item.input
	}
	ids, created, err := s.repo.BatchCreateOrUpdateMessages(ctx, inputs)
	if err != nil {
		return err
	}
	// Patch attachment rows with the now-known message ids, then batch upsert.
	var attachments []domain.Attachment
	for i, item := range pending {
		if len(item.attachments) == 0 {
			continue
		}
		for j := range item.attachments {
			item.attachments[j].MessageID = ids[i]
		}
		attachments = append(attachments, item.attachments...)
	}
	if len(attachments) > 0 {
		if err := s.repo.BatchUpsertAttachments(ctx, attachments); err != nil {
			return err
		}
	}
	for i, item := range pending {
		if !created[i] {
			continue
		}
		data := map[string]any{"message_id": ids[i], "account_id": mailbox.AccountID, "mailbox_id": mailbox.ID}
		// The body is still empty at this point, so only the subject can be
		// scanned. It catches the common "【服务】验证码 123456" shape without
		// waiting for the prefetch; fetchBody re-runs detection on the full
		// text and replaces this notification.
		if item.fetched.Envelope != nil && withinOTPWindow(item.fetched.InternalDate) {
			if code, ok := mailparser.DetectOTP(item.fetched.Envelope.Subject, "", ""); ok {
				data["otp_code"] = code
				data["otp_subject"] = item.fetched.Envelope.Subject
			}
		}
		s.events.Publish(ports.Event{Type: "NEW_EMAIL", Data: data})
	}
	return nil
}

func imapAddresses(input []goimap.Address) []*mail.Address {
	result := make([]*mail.Address, 0, len(input))
	for _, value := range input {
		if value.Addr() != "" {
			result = append(result, &mail.Address{Name: value.Name, Address: value.Addr()})
		}
	}
	return result
}

func encodeMailAddresses(input []*mail.Address) string {
	values := make([]string, 0, len(input))
	for _, value := range input {
		values = append(values, formatAddress(value.Name, value.Address))
	}
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

// specialsInDisplayName are the RFC 5322 "specials" that force a display name to
// be quoted. A bare UTF-8 name needs no quoting, which is what keeps the stored
// form readable.
const specialsInDisplayName = `()<>[]:;@\,."`

// formatAddress renders "Name <addr>" without RFC 2047 encoding. net/mail's
// Address.String() cannot be used here: it re-encodes every non-ASCII display
// name into an encoded-word, so a name go-imap had already decoded came back out
// as a literal "=?utf-8?q?...?=" — visible in the UI and indexed that way by
// FTS5, which also made the readable name unsearchable. Quoting still follows
// RFC 5322 so the result round-trips through mail.ParseAddress when a draft
// built from these values is sent.
func formatAddress(name, address string) string {
	// Whitespace is collapsed to single spaces. A CR or LF here is either a folded
	// header the decoder left behind or an injection attempt; it has to go, but as a
	// space rather than by deletion, so the two sides of the break do not fuse into
	// one word. This value is parsed back and written into an outgoing draft.
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return address
	}
	if strings.ContainsAny(name, specialsInDisplayName) {
		var quoted strings.Builder
		quoted.Grow(len(name) + 2)
		quoted.WriteByte('"')
		for _, r := range name {
			if r == '"' || r == '\\' {
				quoted.WriteByte('\\')
			}
			quoted.WriteRune(r)
		}
		quoted.WriteByte('"')
		name = quoted.String()
	}
	return name + " <" + address + ">"
}

func addresses(input []goimap.Address) []string {
	result := make([]string, 0, len(input))
	for _, value := range input {
		if value.Addr() != "" {
			result = append(result, formatAddress(value.Name, value.Addr()))
		}
	}
	return result
}

func hasFlag(flags []goimap.Flag, target goimap.Flag) bool {
	for _, flag := range flags {
		if flag == target {
			return true
		}
	}
	return false
}
