package imap

import (
	"context"
	"fmt"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Reconciliation repairs what the append-only sync cannot see: flags changed in
// another client, and messages deleted or expunged elsewhere.

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
func (s *Supervisor) reconcileMailbox(ctx context.Context, client *imapclient.Client, mailbox domain.Mailbox, selected *goimap.SelectData) error {
	if mailbox.Role == "drafts" {
		// Drafts have their own reconciliation with conflict handling in
		// ReconcileRemoteDraft; flags and expunges there mean something else.
		return nil
	}
	if value, ok := s.lastReconcile.Load(mailbox.ID); ok {
		if last, valid := value.(time.Time); valid && time.Since(last) < reconcileIntervalFor(mailbox.Role) {
			return nil
		}
	}
	// A UIDVALIDITY change is handled inside syncMailbox (ResetMailbox + the
	// local mailbox.UIDValidity update on the same value-typed struct that
	// flows into this call), so by the time we reach here the validity has
	// already been equalised and there is nothing for an extra check to do.
	stored, err := s.repo.ListMailboxUIDs(ctx, mailbox.ID)
	if err != nil {
		return fmt.Errorf("list local uids: %w", err)
	}
	return s.reconcileMailboxWithUIDs(ctx, client, mailbox, selected, stored)
}

// reconcileMailboxWithUIDs is reconcileMailbox over an explicit snapshot of the
// local UIDs. Splitting it out keeps the snapshot visible as an input, because
// which UIDs were asked about is exactly what decides which ones may be deleted.
func (s *Supervisor) reconcileMailboxWithUIDs(ctx context.Context, client *imapclient.Client, mailbox domain.Mailbox, selected *goimap.SelectData, stored []uint32) error {
	if len(stored) == 0 {
		s.lastReconcile.Store(mailbox.ID, time.Now())
		return nil
	}
	defer observe("reconcile", mailbox.AccountID, "mailbox", mailbox.RemoteName, "local_uids", len(stored))()
	present, changed, err := s.reconcileFlags(ctx, client, mailbox, stored)
	if err != nil {
		return err
	}
	removed, err := s.repo.DeleteMailboxUIDs(ctx, mailbox.ID, staleUIDs(stored, present))
	if err != nil {
		return fmt.Errorf("drop expunged: %w", err)
	}
	s.lastReconcile.Store(mailbox.ID, time.Now())
	if removed == 0 && changed == 0 {
		return nil
	}
	// One aggregate event: a per-message burst would overrun the realtime hub's
	// buffer and disconnect the very client waiting for the correction.
	s.events.Publish(ports.Event{Type: "MESSAGE_UPDATED", Data: map[string]any{
		"bulk": true, "reconciled": changed, "removed": removed, "mailbox_id": mailbox.ID,
	}})
	return nil
}

// staleUIDs returns the UIDs that were asked about and did not come back, which
// is the only safe definition of "expunged" here: anything that arrived after the
// caller's snapshot is simply not part of the pass and must be left alone.
//
// The result is deliberately not pre-sized from len(stored)-len(present). A
// provider is free to echo UIDs that were never in the request — some servers
// answer a chunked UID FETCH with the whole mailbox — which makes that difference
// negative and panics the calling body worker with "makeslice: cap out of range".
func staleUIDs(stored, present []uint32) []uint32 {
	seen := make(map[uint32]struct{}, len(present))
	for _, uid := range present {
		seen[uid] = struct{}{}
	}
	stale := make([]uint32, 0, len(stored))
	for _, uid := range stored {
		if _, ok := seen[uid]; !ok {
			stale = append(stale, uid)
		}
	}
	return stale
}

// reconcileFlags fetches the provider's flags for the UIDs held locally and
// writes back the ones that differ. It returns the UIDs the provider still has,
// which is what the caller uses to find the expunged ones.
//
// CHANGEDSINCE is deliberately not used. It would shrink the response, but a UID
// omitted because its flags did not change is indistinguishable from one that was
// expunged, and treating the first as the second deletes mail that still exists.
// The request is already bounded by the local row count, so the full answer is
// affordable.
func (s *Supervisor) reconcileFlags(ctx context.Context, client *imapclient.Client, mailbox domain.Mailbox, stored []uint32) ([]uint32, int, error) {
	present := make([]uint32, 0, len(stored))
	total := 0
	options := &goimap.FetchOptions{UID: true, Flags: true}
	for start := 0; start < len(stored); start += reconcileFlagChunk {
		end := min(start+reconcileFlagChunk, len(stored))
		chunk := stored[start:end]
		set := make([]goimap.UID, len(chunk))
		for index, uid := range chunk {
			set[index] = goimap.UID(uid)
		}
		fetched, err := client.Fetch(goimap.UIDSetNum(set...), options).Collect()
		if err != nil {
			return nil, total, fmt.Errorf("fetch flags: %w", err)
		}
		states := make([]ports.RemoteFlagState, 0, len(fetched))
		for _, item := range fetched {
			values := make([]string, len(item.Flags))
			for index, flag := range item.Flags {
				values[index] = string(flag)
			}
			present = append(present, uint32(item.UID))
			states = append(states, ports.RemoteFlagState{
				UID:       uint32(item.UID),
				IsRead:    hasFlag(item.Flags, goimap.FlagSeen),
				IsStarred: hasFlag(item.Flags, goimap.FlagFlagged),
				Flags:     values,
			})
		}
		changed, err := s.repo.ReconcileMailboxFlags(ctx, mailbox.ID, states)
		if err != nil {
			return nil, total, fmt.Errorf("apply flags: %w", err)
		}
		total += changed
	}
	return present, total, nil
}
