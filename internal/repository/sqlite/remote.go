package sqlite

import (
	"context"
	"encoding/json"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"

	"gorm.io/gorm"
)

func (s *Store) GetMailboxByRole(ctx context.Context, accountID int64, role string) (domain.Mailbox, error) {
	var mailbox domain.Mailbox
	err := s.db.WithContext(ctx).Where("account_id = ? AND role = ?", accountID, role).Order("id").First(&mailbox).Error
	return mailbox, classifyRead(err)
}

func (s *Store) GetMailbox(ctx context.Context, id int64) (domain.Mailbox, error) {
	var mailbox domain.Mailbox
	err := s.db.WithContext(ctx).First(&mailbox, id).Error
	return mailbox, classifyRead(err)
}

func (s *Store) UpdateMailboxCursor(ctx context.Context, id int64, uidValidity, lastUID uint32, uidNext *uint32, highestModSeq *uint64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now().UnixMilli()
	return s.db.WithContext(ctx).Model(&domain.Mailbox{}).Where("id = ?", id).Updates(map[string]any{
		"uid_validity": uidValidity, "last_uid": lastUID, "uid_next": uidNext,
		"highest_modseq": highestModSeq, "last_sync_at": now, "updated_at": now,
	}).Error
}

func (s *Store) ResetMailbox(ctx context.Context, id int64, uidValidity uint32) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM mailbox_messages WHERE mailbox_id = ?", id).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM messages WHERE id NOT IN (SELECT message_id FROM mailbox_messages) AND direction = 'incoming'").Error; err != nil {
			return err
		}
		return tx.Model(&domain.Mailbox{}).Where("id = ?", id).Updates(map[string]any{
			"uid_validity": uidValidity, "last_uid": 0, "last_sync_at": nil, "updated_at": time.Now().UnixMilli(),
		}).Error
	})
}

// ReconcileMailboxFlags applies the provider's view of \Seen and \Flagged to the
// local rows of one mailbox and returns how many messages actually changed.
//
// Sync only ever appended new UIDs, so a message read or starred in another
// client stayed wrong here forever. Only rows that differ are written, both to
// keep the FTS triggers idle and so the caller can stay silent when nothing moved.
func (s *Store) ReconcileMailboxFlags(ctx context.Context, mailboxID int64, states []ports.RemoteFlagState) (int, error) {
	if len(states) == 0 {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.invalidateUnreadCache()
	changed := 0
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UnixMilli()
		for start := 0; start < len(states); start += sqliteParameterChunk {
			end := min(start+sqliteParameterChunk, len(states))
			chunk := states[start:end]
			uids := make([]uint32, len(chunk))
			for index, state := range chunk {
				uids[index] = state.UID
			}
			type row struct {
				MessageID int64  `gorm:"column:message_id"`
				UID       uint32 `gorm:"column:uid"`
				IsRead    bool   `gorm:"column:is_read"`
				IsStarred bool   `gorm:"column:is_starred"`
				FlagsJSON string `gorm:"column:flags_json"`
			}
			var rows []row
			if err := tx.Raw(`SELECT mm.message_id, mm.uid, m.is_read, m.is_starred, mm.flags_json
                FROM mailbox_messages mm JOIN messages m ON m.id = mm.message_id
                WHERE mm.mailbox_id = ? AND mm.uid IN ?`, mailboxID, uids).Scan(&rows).Error; err != nil {
				return err
			}
			local := make(map[uint32]row, len(rows))
			for _, item := range rows {
				local[item.UID] = item
			}
			for _, state := range chunk {
				current, ok := local[state.UID]
				if !ok {
					continue
				}
				flagsJSON, _ := json.Marshal(state.Flags)
				// Diff-write: a 5-minute tick on a 2000-UID mailbox used to
				// issue 2000 single-row UPDATEs to mailbox_messages even when
				// nothing changed. Skipping the no-op case is one free
				// comparison in memory because the SELECT already returned
				// the row.
				if current.FlagsJSON != string(flagsJSON) {
					if err := tx.Exec("UPDATE mailbox_messages SET flags_json = ? WHERE mailbox_id = ? AND uid = ?", string(flagsJSON), mailboxID, state.UID).Error; err != nil {
						return err
					}
				}
				if current.IsRead == state.IsRead && current.IsStarred == state.IsStarred {
					continue
				}
				if err := tx.Model(&domain.Message{}).Where("id = ?", current.MessageID).Updates(map[string]any{
					"is_read": state.IsRead, "is_starred": state.IsStarred, "updated_at": now,
				}).Error; err != nil {
					return err
				}
				changed++
			}
		}
		return nil
	})
	return changed, err
}

// ListMailboxUIDs returns the UIDs this mailbox holds locally, ascending.
func (s *Store) ListMailboxUIDs(ctx context.Context, mailboxID int64) ([]uint32, error) {
	var uids []uint32
	err := s.db.WithContext(ctx).Raw("SELECT uid FROM mailbox_messages WHERE mailbox_id = ? ORDER BY uid", mailboxID).Scan(&uids).Error
	return uids, err
}

// DeleteMailboxUIDs drops the local mapping for UIDs the provider no longer
// holds, then removes messages that are left in no mailbox at all.
//
// Without this, mail deleted or expunged in another client stayed in the feed
// forever, and opening it produced a body fetch that could never succeed.
//
// The caller passes the UIDs it established are gone rather than the ones that
// remain: deriving the stale set here from "everything not in this list" would
// delete any row inserted after the caller took its snapshot, which for the inbox
// means deleting mail that had just arrived.
// orphanSweepSQL removes messages left in no mailbox by a delete pass, bounded to
// the ids that pass actually unmapped. It is a named constant so the query plan
// test runs against the statement that ships rather than against a copy of it.
const orphanSweepSQL = `DELETE FROM messages WHERE direction = 'incoming' AND id IN ?
    AND NOT EXISTS (SELECT 1 FROM mailbox_messages WHERE message_id = messages.id)`

func (s *Store) DeleteMailboxUIDs(ctx context.Context, mailboxID int64, stale []uint32) (int, error) {
	if len(stale) == 0 {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	removed := 0
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The message ids behind the doomed mappings are read first: after the
		// mappings are gone there is no way to name them, and they are what bounds
		// the orphan sweep below.
		var affected []int64
		for start := 0; start < len(stale); start += sqliteParameterChunk {
			end := min(start+sqliteParameterChunk, len(stale))
			chunk := stale[start:end]
			var ids []int64
			if err := tx.Raw("SELECT message_id FROM mailbox_messages WHERE mailbox_id = ? AND uid IN ?", mailboxID, chunk).Scan(&ids).Error; err != nil {
				return err
			}
			affected = append(affected, ids...)
			result := tx.Exec("DELETE FROM mailbox_messages WHERE mailbox_id = ? AND uid IN ?", mailboxID, chunk)
			if result.Error != nil {
				return result.Error
			}
			removed += int(result.RowsAffected)
		}
		if removed == 0 {
			return nil
		}
		// An incoming message that is in no mailbox is unreachable: it cannot be
		// opened, its body cannot be fetched, and it would sit in the feed as a
		// permanent error. Outgoing mail has no mailbox mapping by design.
		//
		// Scoped to the ids this pass actually unmapped. The earlier form was
		// `id NOT IN (SELECT message_id FROM mailbox_messages)` over the whole
		// messages table, which scanned every message and re-materialised every
		// mapping on each reconcile that expunged even one UID — and it also made
		// the sweep's cost independent of how much was deleted, so the cheapest
		// possible correction paid the full price.
		for start := 0; start < len(affected); start += sqliteParameterChunk {
			end := min(start+sqliteParameterChunk, len(affected))
			if err := tx.Exec(orphanSweepSQL, affected[start:end]).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return removed, err
}

func (s *Store) MessageLocation(ctx context.Context, messageID int64) (ports.MessageLocation, error) {
	var location ports.MessageLocation
	var mapping struct {
		AccountID int64  `gorm:"column:account_id"`
		MailboxID int64  `gorm:"column:mailbox_id"`
		UID       uint32 `gorm:"column:uid"`
	}
	err := s.db.WithContext(ctx).Raw(`SELECT mb.account_id, mm.mailbox_id, mm.uid
        FROM mailbox_messages mm JOIN mailboxes mb ON mb.id = mm.mailbox_id
        WHERE mm.message_id = ?
        ORDER BY CASE mb.role WHEN 'inbox' THEN 0 WHEN 'archive' THEN 1 ELSE 2 END LIMIT 1`, messageID).Scan(&mapping).Error
	if err != nil || mapping.MailboxID == 0 {
		if err == nil {
			err = gorm.ErrRecordNotFound
		}
		return location, classifyRead(err)
	}
	if err = s.db.WithContext(ctx).First(&location.Account, mapping.AccountID).Error; err != nil {
		return location, classifyRead(err)
	}
	if err = s.db.WithContext(ctx).First(&location.Mailbox, mapping.MailboxID).Error; err != nil {
		return location, classifyRead(err)
	}
	location.UID = mapping.UID
	location.MessageID = messageID
	return location, nil
}

// MessageLocations resolves many messages at once. MessageLocation costs three
// round-trips per message, which a bulk flag update would multiply by the whole
// backlog, so accounts and mailboxes are fetched once and joined in memory.
// Messages with no remote mapping are omitted rather than failing the batch.
func (s *Store) MessageLocations(ctx context.Context, messageIDs []int64) ([]ports.MessageLocation, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	type mapping struct {
		MessageID int64  `gorm:"column:message_id"`
		AccountID int64  `gorm:"column:account_id"`
		MailboxID int64  `gorm:"column:mailbox_id"`
		UID       uint32 `gorm:"column:uid"`
	}
	var mappings []mapping
	for start := 0; start < len(messageIDs); start += sqliteParameterChunk {
		end := min(start+sqliteParameterChunk, len(messageIDs))
		var chunk []mapping
		// The window picks one mailbox per message with the same inbox-first
		// preference MessageLocation uses, so single and bulk updates target the
		// same copy of a message that lives in several mailboxes.
		err := s.db.WithContext(ctx).Raw(`SELECT message_id, account_id, mailbox_id, uid FROM (
                SELECT mm.message_id, mb.account_id, mm.mailbox_id, mm.uid,
                    ROW_NUMBER() OVER (PARTITION BY mm.message_id ORDER BY
                        CASE mb.role WHEN 'inbox' THEN 0 WHEN 'archive' THEN 1 ELSE 2 END, mm.mailbox_id) AS rank
                FROM mailbox_messages mm JOIN mailboxes mb ON mb.id = mm.mailbox_id
                WHERE mm.message_id IN ?
            ) WHERE rank = 1`, messageIDs[start:end]).Scan(&chunk).Error
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, chunk...)
	}
	if len(mappings) == 0 {
		return nil, nil
	}
	accountIDs := make(map[int64]struct{}, len(mappings))
	mailboxIDs := make(map[int64]struct{}, len(mappings))
	for _, item := range mappings {
		accountIDs[item.AccountID] = struct{}{}
		mailboxIDs[item.MailboxID] = struct{}{}
	}
	var accountRows []domain.Account
	if err := s.db.WithContext(ctx).Where("id IN ?", keysOf(accountIDs)).Find(&accountRows).Error; err != nil {
		return nil, err
	}
	var mailboxRows []domain.Mailbox
	if err := s.db.WithContext(ctx).Where("id IN ?", keysOf(mailboxIDs)).Find(&mailboxRows).Error; err != nil {
		return nil, err
	}
	accounts := make(map[int64]domain.Account, len(accountRows))
	for _, account := range accountRows {
		accounts[account.ID] = account
	}
	mailboxes := make(map[int64]domain.Mailbox, len(mailboxRows))
	for _, mailbox := range mailboxRows {
		mailboxes[mailbox.ID] = mailbox
	}
	locations := make([]ports.MessageLocation, 0, len(mappings))
	for _, item := range mappings {
		account, hasAccount := accounts[item.AccountID]
		mailbox, hasMailbox := mailboxes[item.MailboxID]
		if !hasAccount || !hasMailbox {
			continue
		}
		locations = append(locations, ports.MessageLocation{MessageID: item.MessageID, Account: account, Mailbox: mailbox, UID: item.UID})
	}
	return locations, nil
}

func keysOf(source map[int64]struct{}) []int64 {
	keys := make([]int64, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	return keys
}

func (s *Store) MoveMessageLocation(ctx context.Context, messageID, sourceMailboxID, destinationMailboxID int64, destinationUID *uint32) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var source struct {
			FlagsJSON    string `gorm:"column:flags_json"`
			InternalDate int64  `gorm:"column:internal_date"`
		}
		if err := tx.Raw("SELECT flags_json, internal_date FROM mailbox_messages WHERE mailbox_id = ? AND message_id = ?", sourceMailboxID, messageID).Scan(&source).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM mailbox_messages WHERE mailbox_id = ? AND message_id = ?", sourceMailboxID, messageID).Error; err != nil {
			return err
		}
		if destinationUID == nil {
			return nil
		}
		return tx.Exec(`INSERT INTO mailbox_messages(mailbox_id, message_id, uid, flags_json, internal_date)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT(mailbox_id, uid) DO UPDATE SET message_id=excluded.message_id, flags_json=excluded.flags_json, internal_date=excluded.internal_date`,
			destinationMailboxID, messageID, *destinationUID, source.FlagsJSON, source.InternalDate).Error
	})
}

func (s *Store) UpdateMessageBody(ctx context.Context, id int64, text, html, snippet string, rawBlobID *int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Model(&domain.Message{}).Where("id = ?", id).Updates(map[string]any{
		"body_text": text, "body_html": html, "snippet": snippet, "body_state": "ready",
		"raw_blob_id": rawBlobID, "updated_at": time.Now().UnixMilli(),
	}).Error
}

func (s *Store) SetMessageBodyState(ctx context.Context, id int64, state string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Model(&domain.Message{}).Where("id = ? AND body_state != 'ready'", id).
		Updates(map[string]any{"body_state": state, "updated_at": time.Now().UnixMilli()}).Error
}

// BatchSetMessageBodyState flips a list of messages to the same body_state in
// a single writeMu section and a single transaction. The hot path is the
// enqueueBodyCandidates loop on the 5s inbox probe: a backlog of 100 body
// candidates used to pay for 100 single-row UPDATEs and 100 fsyncs; the
// batch collapses it to one.
//
// Rows that are already 'ready' are skipped, matching the per-row guard.
// The caller treats a non-nil error as a no-op for the whole batch — it is
// not safe to retry half-applied state because the body queue may already
// hold the IDs.
func (s *Store) BatchSetMessageBodyState(ctx context.Context, ids []int64, state string) error {
	if len(ids) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now().UnixMilli()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for start := 0; start < len(ids); start += sqliteParameterChunk {
			end := min(start+sqliteParameterChunk, len(ids))
			if err := tx.Model(&domain.Message{}).
				Where("id IN ? AND body_state != 'ready'", ids[start:end]).
				Updates(map[string]any{"body_state": state, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// BatchUpsertAttachments upserts a batch of attachments under a single writeMu.
// It reports the first error and bails rather than silently losing rows mid-batch.
func (s *Store) BatchUpsertAttachments(ctx context.Context, attachments []domain.Attachment) error {
	if len(attachments) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i := range attachments {
			att := attachments[i]
			if err := tx.Exec(`INSERT INTO attachments
                (message_id, part_id, filename, content_type, disposition, content_id, size_bytes, fetch_state, blob_id, last_error, created_at, updated_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(message_id, part_id) DO UPDATE SET filename=excluded.filename, content_type=excluded.content_type,
                disposition=excluded.disposition, content_id=excluded.content_id, size_bytes=excluded.size_bytes, updated_at=excluded.updated_at`,
				att.MessageID, att.PartID, att.Filename, att.ContentType, att.Disposition,
				att.ContentID, att.SizeBytes, att.FetchState, att.BlobID, att.LastError,
				att.CreatedAt, att.UpdatedAt).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) GetAttachment(ctx context.Context, messageID, attachmentID int64) (domain.Attachment, error) {
	var attachment domain.Attachment
	err := s.db.WithContext(ctx).Where("id = ? AND message_id = ?", attachmentID, messageID).First(&attachment).Error
	return attachment, classifyRead(err)
}

func (s *Store) UpdateAttachmentBlob(ctx context.Context, id, blobID int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Model(&domain.Attachment{}).Where("id = ?", id).Updates(map[string]any{
		"blob_id": blobID, "fetch_state": "ready", "last_error": nil, "updated_at": time.Now().UnixMilli(),
	}).Error
}

func (s *Store) ListBodyCandidateIDs(ctx context.Context, accountID, maxSize int64, limit int) ([]int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var ids []int64
	err := s.db.WithContext(ctx).Model(&domain.Message{}).
		Where("account_id = ? AND direction = 'incoming' AND body_state != 'ready' AND size_bytes > 0 AND size_bytes <= ?", accountID, maxSize).
		Order("received_at DESC, id DESC").Limit(limit).Pluck("id", &ids).Error
	return ids, err
}
