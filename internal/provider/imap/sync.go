package imap

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
	"nexusmail/internal/provider"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Mailbox synchronisation: the catalog refresh, the per-account pass, and the
// per-mailbox incremental UID walk.

// refreshMailboxCatalog lists the provider's mailboxes and upserts the
// classification. The LIST call is a one-shot round trip and the database writes
// are independent of any other IMAP state, so the sync callers deliberately run
// it *without* the command lock, keeping it off the new-mail path; it is also
// safe to call while holding the lock, which ensureArchiveMailbox does because it
// must not race another writer between LIST and CREATE. The sync path is expected
// to invoke it before syncAllMailboxes so the latter sees a complete catalog.
//
// It returns the LIST entries, because the mailbox attributes are not persisted
// and a caller that needs them — ensureArchiveMailbox looks for \Noselect
// containers — would otherwise have to LIST a second time.
func (s *Supervisor) refreshMailboxCatalog(ctx context.Context, rt *runtime, client *imapclient.Client) ([]*goimap.ListData, error) {
	items, err := client.List("", "*", nil).Collect()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	for _, item := range items {
		attrs := make([]string, len(item.Attrs))
		for i, attr := range item.Attrs {
			attrs[i] = string(attr)
		}
		role, mode := provider.ClassifyMailbox(item.Mailbox, attrs)
		var delimiter *string
		if item.Delim != 0 {
			value := string(item.Delim)
			delimiter = &value
		}
		mailbox := domain.Mailbox{AccountID: rt.account.ID, RemoteName: item.Mailbox, DisplayName: item.Mailbox, Delimiter: delimiter, Role: role, SyncMode: mode, CreatedAt: now, UpdatedAt: now}
		if err := s.repo.UpsertMailbox(ctx, &mailbox); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// syncAllMailboxes iterates the catalog and runs syncMailbox on each non-lazy
// mailbox, draining queued sync requests between them. The caller must hold
// the command lock; each syncMailbox re-selects its own mailbox, so this is
// only safe between mailboxes, never inside one.

// syncAllMailboxes iterates the catalog and runs syncMailbox on each non-lazy
// mailbox, draining queued sync requests between them. The caller must hold
// the command lock; each syncMailbox re-selects its own mailbox, so this is
// only safe between mailboxes, never inside one.
func (s *Supervisor) syncAllMailboxes(ctx context.Context, rt *runtime, client *imapclient.Client) error {
	defer observe("sync_all", rt.account.ID)()
	mailboxes, err := s.repo.ListMailboxes(ctx, rt.account.ID)
	if err != nil {
		return err
	}
	for _, mailbox := range mailboxes {
		if mailbox.SyncMode == "lazy" {
			continue
		}
		// Sent, drafts and archive change rarely, but syncing them cost a SELECT and
		// a UID SEARCH each regardless, on the same connection new mail needs. STATUS
		// answers "did anything move" in one command, so a quiet mailbox is skipped
		// unless reconciliation is due — which is the only thing that can change
		// without UIDNEXT changing.
		if mailbox.Role != "inbox" && s.mailboxQuiet(client, mailbox) {
			continue
		}
		if err := s.syncMailbox(ctx, client, mailbox, false); err != nil {
			return fmt.Errorf("sync %s: %w", mailbox.RemoteName, err)
		}
		// A full sync can span minutes on large accounts. Serve queued inbox
		// signals between mailboxes so new mail is not stuck behind it.
		if err := s.drainPending(ctx, rt, client); err != nil {
			return err
		}
	}
	return nil
}

// mailboxQuiet reports whether a mailbox can be skipped this pass: nothing new
// arrived and its reconciliation is not yet due. A STATUS failure returns false so
// the caller falls back to the full path rather than silently skipping a mailbox.

// mailboxQuiet reports whether a mailbox can be skipped this pass: nothing new
// arrived and its reconciliation is not yet due. A STATUS failure returns false so
// the caller falls back to the full path rather than silently skipping a mailbox.
func (s *Supervisor) mailboxQuiet(client *imapclient.Client, mailbox domain.Mailbox) bool {
	if mailbox.UIDNext == nil || mailbox.UIDValidity == 0 {
		return false
	}
	value, ok := s.lastReconcile.Load(mailbox.ID)
	last, valid := value.(time.Time)
	if !ok || !valid || time.Since(last) >= reconcileIntervalFor(mailbox.Role) {
		return false
	}
	status, err := client.Status(mailbox.RemoteName, &goimap.StatusOptions{UIDNext: true, UIDValidity: true}).Wait()
	if err != nil {
		slog.Debug("mailbox status probe failed", "mailbox_id", mailbox.ID, "error", err)
		return false
	}
	return status.UIDNext != 0 && uint32(status.UIDNext) == *mailbox.UIDNext && status.UIDValidity == mailbox.UIDValidity
}

// drainPending services queued sync requests. The caller must hold the command
// lock; each syncMailbox re-selects its own mailbox, so this is only safe
// between mailboxes, never inside one.

// drainPending services queued sync requests. The caller must hold the command
// lock; each syncMailbox re-selects its own mailbox, so this is only safe
// between mailboxes, never inside one.
func (s *Supervisor) drainPending(ctx context.Context, rt *runtime, client *imapclient.Client) error {
	for {
		select {
		case mailboxID := <-rt.syncReq:
			if mailboxID == 0 {
				if err := s.syncRole(ctx, client, rt.account.ID, "inbox", false); err != nil {
					return err
				}
				continue
			}
			mailbox, err := s.repo.GetMailbox(ctx, mailboxID)
			if err != nil {
				return err
			}
			if mailbox.AccountID != rt.account.ID {
				continue
			}
			if err := s.syncMailbox(ctx, client, mailbox, false); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (s *Supervisor) syncRole(ctx context.Context, client *imapclient.Client, accountID int64, role string, skipReconcile bool) error {
	mailbox, err := s.repo.GetMailboxByRole(ctx, accountID, role)
	if err != nil {
		return err
	}
	return s.syncMailbox(ctx, client, mailbox, skipReconcile)
}

// incrementalUIDRange returns the UID range to search for mail newer than the
// stored cursor, and whether a search is worth sending at all.
//
// The upper bound comes from the UIDNEXT that SELECT just reported, which is the
// only bound that is both finite and true. The two alternatives are both known
// to break:
//
//   - the 0="*" sentinel produces "3355:*", and a server that resolves "*" to a
//     UID below the start normalises the reversed range per RFC 3501 — so an
//     up-to-date mailbox re-fetched its newest message on every 5-second probe.
//   - math.MaxUint32 produces "3355:4294967295", which QQ refuses with
//     "NO System busy!" on UID SEARCH while accepting SELECT on the same
//     connection. That reply is classified as a throttle, so the account parked
//     in backoff and stopped syncing entirely while looking merely rate-limited.
//
// When the cursor already covers UIDNEXT-1 there is nothing to ask about, and
// skipping the round trip is what keeps the 5-second probe cheap. A server that
// omits UIDNEXT leaves only the sentinel, which is still better than a bound
// that a provider rejects outright.

// incrementalUIDRange returns the UID range to search for mail newer than the
// stored cursor, and whether a search is worth sending at all.
//
// The upper bound comes from the UIDNEXT that SELECT just reported, which is the
// only bound that is both finite and true. The two alternatives are both known
// to break:
//
//   - the 0="*" sentinel produces "3355:*", and a server that resolves "*" to a
//     UID below the start normalises the reversed range per RFC 3501 — so an
//     up-to-date mailbox re-fetched its newest message on every 5-second probe.
//   - math.MaxUint32 produces "3355:4294967295", which QQ refuses with
//     "NO System busy!" on UID SEARCH while accepting SELECT on the same
//     connection. That reply is classified as a throttle, so the account parked
//     in backoff and stopped syncing entirely while looking merely rate-limited.
//
// When the cursor already covers UIDNEXT-1 there is nothing to ask about, and
// skipping the round trip is what keeps the 5-second probe cheap. A server that
// omits UIDNEXT leaves only the sentinel, which is still better than a bound
// that a provider rejects outright.
func incrementalUIDRange(lastUID uint32, uidNext goimap.UID) (goimap.UIDSet, bool) {
	var set goimap.UIDSet
	start := goimap.UID(lastUID) + 1
	if uidNext == 0 {
		set.AddRange(start, 0)
		return set, true
	}
	if uidNext <= start {
		return set, false
	}
	set.AddRange(start, uidNext-1)
	return set, true
}

// syncMailbox ingests new UIDs and, unless skipReconcile is set, repairs
// flag/expunge drift. The 5s inbox probe passes skipReconcile=true: the safety
// net's only job is to surface new mail quickly, and reconciliation on a large
// inbox holds the command connection for the time the probe is supposed to
// save. Drift is still caught by the 5-minute periodic sync, so the worst case
// is a 5-minute delay on flag changes or remote expunges — the same as before
// the safety net existed at all.

// syncMailbox ingests new UIDs and, unless skipReconcile is set, repairs
// flag/expunge drift. The 5s inbox probe passes skipReconcile=true: the safety
// net's only job is to surface new mail quickly, and reconciliation on a large
// inbox holds the command connection for the time the probe is supposed to
// save. Drift is still caught by the 5-minute periodic sync, so the worst case
// is a 5-minute delay on flag changes or remote expunges — the same as before
// the safety net existed at all.
func (s *Supervisor) syncMailbox(ctx context.Context, client *imapclient.Client, mailbox domain.Mailbox, skipReconcile bool) error {
	defer observe("sync_mailbox", mailbox.AccountID, "mailbox", mailbox.RemoteName, "role", mailbox.Role)()
	// CONDSTORE is requested so SELECT reports HIGHESTMODSEQ and the cursor can
	// record it. Nothing reads it back yet — reconciliation cannot narrow on it
	// without losing the ability to tell an expunge from an unchanged flag, see
	// reconcileFlags — but it is the anchor any future QRESYNC path needs, and it is
	// only useful if it was being recorded all along.
	condStore := client.Caps().Has(goimap.CapCondStore)
	selected, err := client.Select(mailbox.RemoteName, &goimap.SelectOptions{ReadOnly: true, CondStore: condStore}).Wait()
	if err != nil {
		// Naming the command matters when a provider rejects only some of them:
		// a bare "sync INBOX: NO System busy!" cannot be told apart from a
		// failing search or fetch, which is the difference between "the account
		// is throttled" and "this one command is refused".
		return fmt.Errorf("select: %w", err)
	}
	if mailbox.UIDValidity != 0 && mailbox.UIDValidity != selected.UIDValidity {
		if err := s.repo.ResetMailbox(ctx, mailbox.ID, selected.UIDValidity); err != nil {
			return err
		}
		mailbox.LastUID = 0
	}
	mailbox.UIDValidity = selected.UIDValidity
	criteria := &goimap.SearchCriteria{}
	var uids []goimap.UID
	if mailbox.LastUID == 0 {
		criteria.Since = time.Now().AddDate(0, 0, -30)
		search, err := client.UIDSearch(criteria, nil).Wait()
		if err != nil {
			return fmt.Errorf("uid search: %w", err)
		}
		uids = search.AllUIDs()
	} else if set, search := incrementalUIDRange(mailbox.LastUID, selected.UIDNext); search {
		criteria.UID = []goimap.UIDSet{set}
		result, err := client.UIDSearch(criteria, nil).Wait()
		if err != nil {
			return fmt.Errorf("uid search: %w", err)
		}
		uids = result.AllUIDs()
	}
	lastUID := mailbox.LastUID
	// pending collects a chunk of built messages so the flush at the end of the
	// chunk can write them under one writeMu + one transaction. Without this
	// batching, a 100-message syncMailbox pass would pay 100 commits.
	var pending []pendingMessage
	for start := 0; start < len(uids); start += 100 {
		end := min(start+100, len(uids))
		fetchOptions := &goimap.FetchOptions{
			UID: true, Envelope: true, Flags: true, InternalDate: true, RFC822Size: true,
			BodyStructure: &goimap.FetchItemBodyStructure{Extended: true},
		}
		messages, err := client.Fetch(goimap.UIDSetNum(uids[start:end]...), fetchOptions).Collect()
		if err != nil {
			return fmt.Errorf("fetch envelopes: %w", err)
		}
		for _, fetched := range messages {
			if mailbox.Role == "drafts" {
				var raw []byte
				if fetched.RFC822Size > 0 && fetched.RFC822Size <= maxInlineDraftImportBytes {
					section := &goimap.FetchItemBodySection{Peek: true}
					bodies, fetchErr := client.Fetch(goimap.UIDSetNum(fetched.UID), &goimap.FetchOptions{UID: true, BodySection: []*goimap.FetchItemBodySection{section}}).Collect()
					if fetchErr == nil && len(bodies) > 0 {
						raw = bodies[0].FindBodySection(section)
					}
				}
				changed, draftID, err := s.storeRemoteDraft(ctx, mailbox, fetched, raw)
				if err != nil {
					if ctx.Err() != nil {
						return err
					}
					// Failing the whole mailbox here would abort before the cursor
					// advances, so one unstorable draft would stall every sync for
					// the account, including the inbox. Skip just this message.
					slog.Error("remote draft import failed", "account_id", mailbox.AccountID, "mailbox_id", mailbox.ID, "uid", fetched.UID, "error", err)
					if uint32(fetched.UID) > lastUID {
						lastUID = uint32(fetched.UID)
					}
					continue
				}
				if changed {
					s.events.Publish(ports.Event{Type: "DRAFT_UPDATED", Data: map[string]any{"draft_id": draftID, "account_id": mailbox.AccountID, "remote": true}})
				}
				if uint32(fetched.UID) > lastUID {
					lastUID = uint32(fetched.UID)
				}
				continue
			}
			// Build a MessageInput and its attachments in memory, then commit the
			// whole chunk to the store at once. The previous shape paid one
			// writeMu and one commit per message; a 100-message sync paid 100
			// commits, and WAL fsync is the dominant cost on the new-mail path.
			input, attachments, err := s.buildFetchedMessage(mailbox, fetched)
			if err != nil {
				return err
			}
			pending = append(pending, pendingMessage{input: input, attachments: attachments, fetched: fetched, uid: uint32(fetched.UID)})
			if uint32(fetched.UID) > lastUID {
				lastUID = uint32(fetched.UID)
			}
		}
		if len(pending) > 0 {
			if err := s.flushPending(ctx, mailbox, pending); err != nil {
				return err
			}
			pending = pending[:0]
		}
	}
	// Appending new UIDs is only half of sync: flags set elsewhere and messages
	// deleted elsewhere are invisible to the pass above. Skip this on the 5s
	// inbox probe — see the skipReconcile comment on syncMailbox.
	if !skipReconcile {
		if err := s.reconcileMailbox(ctx, client, mailbox, selected); err != nil {
			// Reconciliation is a repair pass, not the ingest path. Failing the mailbox
			// here would also discard the new mail just stored and stop the cursor from
			// advancing, so the error is recorded and the sync still commits.
			slog.Warn("mailbox reconcile failed", "account_id", mailbox.AccountID, "mailbox_id", mailbox.ID, "error", err)
		}
	}
	uidNext := uint32(selected.UIDNext)
	highest := selected.HighestModSeq
	return s.repo.UpdateMailboxCursor(ctx, mailbox.ID, selected.UIDValidity, lastUID, &uidNext, &highest)
}

// reconcileMailbox brings local rows back in line with the provider for changes
// the append-only pass cannot observe: \Seen and \Flagged set in another client,
// and messages deleted or expunged elsewhere.
//
// The mailbox must already be selected by the caller, which also holds the
// command lock, so everything this does is charged to new-mail latency. It is
// driven from the UIDs stored locally: one chunked UID FETCH of flags over that
// list answers both questions at once, because a UID the provider no longer has
// is simply absent from the response.
//
// The earlier shape asked the provider instead — UID SEARCH ALL plus a flag FETCH
// over everything it returned — which made the cost scale with the remote mailbox
// rather than with what the app holds. On a Gmail "All Mail" or a long-lived QQ
// inbox that is a six-figure UID list and a multi-megabyte flag response pulled
// under the foreground lock every five minutes, and new mail waited behind it.
