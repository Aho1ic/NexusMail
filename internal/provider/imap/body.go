package imap

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"nexusmail/internal/domain"
	mailparser "nexusmail/internal/mail"
	"nexusmail/internal/ports"

	goimap "github.com/emersion/go-imap/v2"
)

// Body and attachment fetching, including the background prefetch workers.

// FetchBody retrieves a message body for a waiting caller and takes priority
// over background prefetch.
func (s *Supervisor) FetchBody(ctx context.Context, messageID int64) error {
	_, err := s.fetchBody(ctx, messageID, false)
	return err
}

// otpNotice carries what a verification-code notification needs. The subject
// travels with the code so the browser can say which service the code is for
// without another round-trip.

// otpNotice carries what a verification-code notification needs. The subject
// travels with the code so the browser can say which service the code is for
// without another round-trip.
type otpNotice struct {
	Code    string
	Subject string
}

// fetchBody returns any verification code the body carries instead of publishing
// it, because this runs while holding the account's command connection and the
// new-mail latency budget leaves no room for extra work under that lock.

// fetchBody returns any verification code the body carries instead of publishing
// it, because this runs while holding the account's command connection and the
// new-mail latency budget leaves no room for extra work under that lock.
func (s *Supervisor) fetchBody(ctx context.Context, messageID int64, background bool) (otp otpNotice, resultErr error) {
	select {
	case s.bodySlots <- struct{}{}:
		defer func() { <-s.bodySlots }()
	case <-ctx.Done():
		return otpNotice{}, ctx.Err()
	}
	message, _, err := s.repo.GetMessage(ctx, messageID)
	if err != nil {
		return otpNotice{}, err
	}
	if message.BodyState == "ready" {
		return otpNotice{}, nil
	}
	if err := s.repo.SetMessageBodyState(ctx, messageID, "fetching"); err != nil {
		return otpNotice{}, err
	}
	defer func() {
		if resultErr != nil {
			_ = s.repo.SetMessageBodyState(context.Background(), messageID, "error")
		}
	}()
	location, err := s.repo.MessageLocation(ctx, messageID)
	if err != nil {
		return otpNotice{}, err
	}
	rt, err := s.runtime(location.Account.ID)
	if err != nil {
		return otpNotice{}, err
	}
	if background {
		if !rt.lockBackground(ctx) {
			return otpNotice{}, ctx.Err()
		}
	} else {
		rt.lock()
	}
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return otpNotice{}, ports.Unavailablef("account is offline")
	}
	if _, err := client.Select(location.Mailbox.RemoteName, &goimap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return otpNotice{}, err
	}
	section := &goimap.FetchItemBodySection{Peek: true}
	items, err := client.Fetch(goimap.UIDSetNum(goimap.UID(location.UID)), &goimap.FetchOptions{UID: true, BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
	if err != nil || len(items) == 0 {
		if err == nil {
			err = errors.New("message body not found")
		}
		return otpNotice{}, err
	}
	body := items[0].FindBodySection(section)
	parsed, err := mailparser.Parse(bytes.NewReader(body))
	if err != nil {
		return otpNotice{}, err
	}
	blob, err := s.blobs.Put(ctx, bytes.NewReader(body), "cache")
	if err != nil {
		return otpNotice{}, err
	}
	if err := s.repo.UpdateMessageBody(ctx, messageID, parsed.Text, parsed.HTML, parsed.Snippet, &blob.ID); err != nil {
		return otpNotice{}, err
	}
	if !withinOTPWindow(time.UnixMilli(message.ReceivedAt)) {
		return otpNotice{}, nil
	}
	code, ok := mailparser.DetectOTP(message.Subject, parsed.Text, parsed.HTML)
	if !ok {
		return otpNotice{}, nil
	}
	return otpNotice{Code: code, Subject: message.Subject}, nil
}

// withinOTPWindow reports whether a message is recent enough that surfacing its
// verification code as a notification is still useful.

// withinOTPWindow reports whether a message is recent enough that surfacing its
// verification code as a notification is still useful.
func withinOTPWindow(received time.Time) bool {
	return !received.IsZero() && time.Since(received) <= otpFreshness
}

func (s *Supervisor) enqueueBodyCandidates(ctx context.Context, accountID int64) {
	ids, err := s.repo.ListBodyCandidateIDs(ctx, accountID, maxInlineDraftImportBytes, 100)
	if err != nil {
		return
	}
	// Pre-filter: the candidate query cannot exclude 'error' or already-seen
	// rows without a schema change, and the bodyAttempts cap is enforced here
	// so a body that has already failed maxBodyAttempts times is not retried
	// for the life of the process. Only the survivors are flipped to 'queued'
	// in one batch write instead of N single-row UPDATEs.
	candidates := make([]int64, 0, len(ids))
	for _, id := range ids {
		if value, ok := s.bodyAttempts.Load(id); ok {
			if attempts, valid := value.(int); valid && attempts >= maxBodyAttempts {
				continue
			}
		}
		if _, loaded := s.bodySeen.LoadOrStore(id, struct{}{}); loaded {
			continue
		}
		candidates = append(candidates, id)
	}
	if len(candidates) == 0 {
		return
	}
	if err := s.repo.BatchSetMessageBodyState(ctx, candidates, "queued"); err != nil {
		for _, id := range candidates {
			s.bodySeen.Delete(id)
		}
		return
	}
	for _, id := range candidates {
		select {
		case s.bodyQueue <- id:
		case <-ctx.Done():
			s.bodySeen.Delete(id)
			_ = s.repo.SetMessageBodyState(context.Background(), id, "metadata")
			return
		default:
			s.bodySeen.Delete(id)
			_ = s.repo.SetMessageBodyState(context.Background(), id, "metadata")
			return
		}
	}
}

func (s *Supervisor) bodyWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.bodyQueue:
			fetchCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			otp, err := s.fetchBody(fetchCtx, id, true)
			cancel()
			s.bodySeen.Delete(id)
			s.recordBodyAttempt(id, err)
			if err == nil {
				// Published after fetchBody released the command lock. The code is
				// carried on the event so the browser can raise a copyable
				// notification without another round-trip.
				data := map[string]any{"message_id": id}
				if otp.Code != "" {
					data["otp_code"] = otp.Code
					data["otp_subject"] = otp.Subject
				}
				s.events.Publish(ports.Event{Type: "MESSAGE_UPDATED", Data: data})
			} else if !errors.Is(err, context.Canceled) {
				// Step aside briefly on failure so a flaky provider does not eat
				// every body slot in a tight loop. maxBodyAttempts already caps the
				// total attempts per message, this just spreads them out.
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
		}
	}
}

// recordBodyAttempt maintains the per-message failure count the prefetch cap
// reads. A success clears the entry so a message that recovers is not held
// against its earlier failures. Cancellation is not a failure of the message: the
// process is shutting down or the caller went away.

// recordBodyAttempt maintains the per-message failure count the prefetch cap
// reads. A success clears the entry so a message that recovers is not held
// against its earlier failures. Cancellation is not a failure of the message: the
// process is shutting down or the caller went away.
func (s *Supervisor) recordBodyAttempt(id int64, err error) {
	if err == nil {
		s.bodyAttempts.Delete(id)
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	attempts := 0
	if value, ok := s.bodyAttempts.Load(id); ok {
		if current, valid := value.(int); valid {
			attempts = current
		}
	}
	s.bodyAttempts.Store(id, attempts+1)
}

func (s *Supervisor) FetchAttachment(ctx context.Context, messageID, attachmentID int64) (domain.BlobObject, domain.Attachment, error) {
	attachment, err := s.repo.GetAttachment(ctx, messageID, attachmentID)
	if err != nil {
		return domain.BlobObject{}, attachment, err
	}
	if attachment.BlobID != nil {
		blob, err := s.repo.GetBlob(ctx, *attachment.BlobID)
		return blob, attachment, err
	}
	location, err := s.repo.MessageLocation(ctx, messageID)
	if err != nil {
		return domain.BlobObject{}, attachment, err
	}
	rt, err := s.runtime(location.Account.ID)
	if err != nil {
		return domain.BlobObject{}, attachment, err
	}
	path, err := parsePartID(attachment.PartID)
	if err != nil {
		return domain.BlobObject{}, attachment, err
	}
	rt.lock()
	defer rt.unlock()
	client := rt.client.Load()
	if client == nil {
		return domain.BlobObject{}, attachment, ports.Unavailablef("account is offline")
	}
	if _, err := client.Select(location.Mailbox.RemoteName, &goimap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return domain.BlobObject{}, attachment, err
	}
	section := &goimap.FetchItemBodySection{Part: path, Peek: true}
	items, err := client.Fetch(goimap.UIDSetNum(goimap.UID(location.UID)), &goimap.FetchOptions{UID: true, BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
	if err != nil || len(items) == 0 {
		if err == nil {
			err = ports.NotFoundf("attachment not found")
		}
		return domain.BlobObject{}, attachment, err
	}
	blob, err := s.blobs.Put(ctx, bytes.NewReader(items[0].FindBodySection(section)), "cache")
	if err != nil {
		return domain.BlobObject{}, attachment, err
	}
	if err := s.repo.UpdateAttachmentBlob(ctx, attachment.ID, blob.ID); err != nil {
		return domain.BlobObject{}, attachment, err
	}
	attachment.BlobID = &blob.ID
	attachment.FetchState = "ready"
	return blob, attachment, nil
}

func parsePartID(value string) ([]int, error) {
	parts := strings.Split(value, ".")
	result := make([]int, len(parts))
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			return nil, ports.Invalidf("invalid attachment part")
		}
		result[i] = n
	}
	return result, nil
}
