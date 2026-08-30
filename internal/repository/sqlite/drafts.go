package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"

	"gorm.io/gorm"
)

func (s *Store) CreateDraft(ctx context.Context, draft *domain.Draft) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Create(draft).Error
}

func (s *Store) ListDrafts(ctx context.Context, status string) ([]domain.Draft, error) {
	var drafts []domain.Draft
	query := s.db.WithContext(ctx).Model(&domain.Draft{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	err := query.Order("updated_at DESC, id DESC").Find(&drafts).Error
	return drafts, err
}

func (s *Store) GetDraft(ctx context.Context, id int64) (domain.Draft, []domain.DraftAttachment, error) {
	var draft domain.Draft
	if err := s.db.WithContext(ctx).First(&draft, id).Error; err != nil {
		return draft, nil, classifyRead(err)
	}
	var attachments []domain.DraftAttachment
	err := s.db.WithContext(ctx).Where("draft_id = ?", id).Order("position, id").Find(&attachments).Error
	return draft, attachments, err
}

func (s *Store) UpdateDraft(ctx context.Context, draft *domain.Draft, expectedRevision int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result := s.db.WithContext(ctx).Model(&domain.Draft{}).
		Where("id = ? AND revision = ? AND status IN ('draft', 'failed', 'unknown')", draft.ID, expectedRevision).
		Updates(map[string]any{
			"to_json": draft.ToJSON, "cc_json": draft.CCJSON, "bcc_json": draft.BCCJSON,
			"subject": draft.Subject, "body_text": draft.BodyText,
			"revision": expectedRevision + 1, "remote_sync_state": "dirty",
			"updated_at": draft.UpdatedAt,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrConflict
	}
	draft.Revision = expectedRevision + 1
	return nil
}

// ReconcileRemoteDraft applies last-writer-wins using IMAP INTERNALDATE. A tie
// inside the five-second clock-skew window is preserved as a conflict copy.
func (s *Store) ReconcileRemoteDraft(ctx context.Context, remote *domain.Draft) (domain.Draft, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var result domain.Draft
	changed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var local domain.Draft
		err := tx.Where("account_id = ? AND rfc_message_id = ?", remote.AccountID, remote.RFCMessageID).First(&local).Error
		if errors.Is(err, gorm.ErrRecordNotFound) && remote.RemoteMailboxID != nil && remote.RemoteUID != nil {
			// Some providers drop or rewrite Message-Id on APPEND, so a draft we
			// uploaded comes back under a different RFC id and looks new. The
			// remote UID still identifies the row and carries a unique index, so
			// match on it before inserting a duplicate that cannot be stored.
			err = tx.Where("remote_mailbox_id = ? AND remote_uid = ?", *remote.RemoteMailboxID, *remote.RemoteUID).First(&local).Error
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if err := tx.Create(remote).Error; err != nil {
				return err
			}
			result, changed = *remote, true
			return nil
		}
		if err != nil {
			return err
		}
		remoteTime := remote.UpdatedAt
		if remote.RemoteUpdatedAt != nil {
			remoteTime = *remote.RemoteUpdatedAt
		}
		if local.RemoteUID != nil && remote.RemoteUID != nil && *local.RemoteUID == *remote.RemoteUID && local.RemoteUIDValidity != nil && remote.RemoteUIDValidity != nil && *local.RemoteUIDValidity == *remote.RemoteUIDValidity {
			result = local
			return nil
		}
		delta := local.UpdatedAt - remoteTime
		if delta < 0 {
			delta = -delta
		}
		if local.RemoteSyncState == "dirty" && delta <= 5_000 {
			conflictOf := local.ID
			copy := *remote
			copy.ID = 0
			copy.ConflictOfID = &conflictOf
			copy.RFCMessageID = fmt.Sprintf("<conflict-%d-%d@nexusmail.local>", local.ID, time.Now().UnixNano())
			copy.RemoteSyncState = "conflict"
			copy.Subject = "[冲突副本] " + copy.Subject
			// The remote UID maps to exactly one row and the dirty local draft keeps
			// it, so the copy is stored as a local-only snapshot instead.
			copy.RemoteMailboxID, copy.RemoteUIDValidity, copy.RemoteUID = nil, nil, nil
			if err := tx.Create(&copy).Error; err != nil {
				return err
			}
			result, changed = copy, true
			return nil
		}
		if local.RemoteSyncState == "dirty" && local.UpdatedAt > remoteTime {
			result = local
			return nil
		}
		updates := map[string]any{
			"to_json": remote.ToJSON, "cc_json": remote.CCJSON, "bcc_json": remote.BCCJSON,
			"subject": remote.Subject, "body_text": remote.BodyText,
			"remote_mailbox_id": remote.RemoteMailboxID, "remote_uid_validity": remote.RemoteUIDValidity,
			"remote_uid": remote.RemoteUID, "remote_updated_at": remote.RemoteUpdatedAt,
			"remote_sync_state": "synced", "revision": local.Revision + 1, "updated_at": remoteTime,
		}
		if err := tx.Model(&local).Updates(updates).Error; err != nil {
			return err
		}
		if err := tx.First(&result, local.ID).Error; err != nil {
			return err
		}
		changed = true
		return nil
	})
	return result, changed, err
}

func (s *Store) UpdateDraftRemote(ctx context.Context, id, mailboxID int64, uidValidity, uid uint32, remoteUpdatedAt int64, state string, lastError *string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Model(&domain.Draft{}).Where("id = ?", id).Updates(map[string]any{
		"remote_mailbox_id": mailboxID, "remote_uid_validity": uidValidity, "remote_uid": uid,
		"remote_updated_at": remoteUpdatedAt, "remote_sync_state": state, "last_error": lastError,
	}).Error
}

func (s *Store) DeleteDraft(ctx context.Context, id int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var draft domain.Draft
		if err := tx.First(&draft, id).Error; err != nil {
			return classifyRead(err)
		}
		if draft.Status == "sending" || draft.Status == "queued" {
			return ErrConflict
		}
		return tx.Delete(&draft).Error
	})
}

func (s *Store) CreateBlob(ctx context.Context, blob *domain.BlobObject) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var existing domain.BlobObject
	err := s.db.WithContext(ctx).Where("sha256 = ?", blob.SHA256).First(&existing).Error
	if err == nil {
		blob.ID = existing.ID
		blob.StorageKey = existing.StorageKey
		if blob.Durability == "durable" && existing.Durability != "durable" {
			return s.db.WithContext(ctx).Model(&existing).Updates(map[string]any{"durability": "durable", "last_accessed_at": blob.LastAccessedAt}).Error
		}
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return s.db.WithContext(ctx).Create(blob).Error
}

func (s *Store) GetBlob(ctx context.Context, id int64) (domain.BlobObject, error) {
	var blob domain.BlobObject
	err := s.db.WithContext(ctx).First(&blob, id).Error
	if err != nil {
		return blob, classifyRead(err)
	}
	// Hold writeMu for the LRU bookkeeping: every other write path does, and
	// the row is the input to the eviction decision. Without the lock, a
	// concurrent delete or upsert of the same blob can race the read and the
	// WAL+8 connection pool can return SQLITE_BUSY. The lookup itself does
	// not need the lock, but the touch does.
	s.writeMu.Lock()
	touchErr := s.db.WithContext(ctx).Model(&blob).Update("last_accessed_at", time.Now().UnixMilli()).Error
	s.writeMu.Unlock()
	if touchErr != nil {
		// The read succeeded; surfacing a write-only error would force callers
		// to retry a fetch that already has the data they need. Log and return
		// the blob as-is: the next read will re-touch the row.
		slog.Warn("touch blob last_accessed_at", "blob_id", id, "error", touchErr)
	}
	return blob, nil
}

func (s *Store) DeleteBlob(ctx context.Context, id int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Delete(&domain.BlobObject{}, id).Error
}

func (s *Store) CachedBlobs(ctx context.Context) ([]domain.BlobObject, error) {
	var blobs []domain.BlobObject
	err := s.db.WithContext(ctx).Where("durability = 'cache'").Order("last_accessed_at ASC").Find(&blobs).Error
	return blobs, err
}

// CachedBlobBytes totals the cache tier without materialising it. Eviction runs
// after every cached write, and loading every row just to add up sizes turned a
// body prefetch over a large backlog into a full scan per message.
func (s *Store) CachedBlobBytes(ctx context.Context) (int64, error) {
	var total *int64
	err := s.db.WithContext(ctx).Model(&domain.BlobObject{}).Where("durability = 'cache'").
		Select("sum(size_bytes)").Scan(&total).Error
	if err != nil || total == nil {
		return 0, err
	}
	return *total, nil
}

func (s *Store) AddDraftAttachment(ctx context.Context, attachment *domain.DraftAttachment) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Create(attachment).Error
}

func (s *Store) DeleteDraftAttachment(ctx context.Context, draftID, attachmentID int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result := s.db.WithContext(ctx).Where("id = ? AND draft_id = ?", attachmentID, draftID).Delete(&domain.DraftAttachment{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		// Classified, so the transport answers 404. A bare gorm.ErrRecordNotFound
		// is unclassified and would be reported as a redacted 500, telling the
		// client its request failed for an unknown reason rather than that the
		// attachment does not exist — or does not belong to this draft.
		return ports.NotFoundf("%w", gorm.ErrRecordNotFound)
	}
	return nil
}

func (s *Store) CreateSession(ctx context.Context, tokenHash, csrfHash []byte, expiresAt, absoluteExpiresAt int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now().UnixMilli()
	_, err := s.sqlDB.ExecContext(ctx, `INSERT INTO web_sessions
        (token_hash, csrf_hash, expires_at, absolute_expires_at, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?)`, tokenHash, csrfHash, expiresAt, absoluteExpiresAt, now, now)
	return err
}

func (s *Store) ValidateSession(ctx context.Context, tokenHash []byte, now int64) ([]byte, bool, error) {
	var row struct {
		CSRFHash          []byte `gorm:"column:csrf_hash"`
		ExpiresAt         int64  `gorm:"column:expires_at"`
		AbsoluteExpiresAt int64  `gorm:"column:absolute_expires_at"`
	}
	err := s.sqlDB.QueryRowContext(ctx, "SELECT csrf_hash, expires_at, absolute_expires_at FROM web_sessions WHERE token_hash = ?", tokenHash).
		Scan(&row.CSRFHash, &row.ExpiresAt, &row.AbsoluteExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if row.ExpiresAt == 0 || row.ExpiresAt <= now || row.AbsoluteExpiresAt <= now {
		return nil, false, nil
	}
	return row.CSRFHash, true, nil
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.sqlDB.ExecContext(ctx, "DELETE FROM web_sessions WHERE token_hash = ?", tokenHash)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context, now int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Exec("DELETE FROM web_sessions WHERE expires_at <= ? OR absolute_expires_at <= ?", now, now).Error
}

func (s *Store) ClaimSendableDraft(ctx context.Context, id int64) (domain.Draft, []domain.DraftAttachment, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var draft domain.Draft
	var attachments []domain.DraftAttachment
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&draft, id).Error; err != nil {
			return err
		}
		if draft.Status != "queued" && draft.Status != "retry_wait" {
			return ErrConflict
		}
		result := tx.Model(&domain.Draft{}).Where("id = ? AND status = ?", id, draft.Status).Updates(map[string]any{
			"status": "sending", "attempt_count": gorm.Expr("attempt_count + 1"), "updated_at": time.Now().UnixMilli(),
		})
		if result.Error != nil || result.RowsAffected != 1 {
			if result.Error != nil {
				return result.Error
			}
			return ErrConflict
		}
		draft.Status = "sending"
		draft.AttemptCount++
		return tx.Where("draft_id = ?", id).Order("position, id").Find(&attachments).Error
	})
	return draft, attachments, err
}

func (s *Store) ListDueDraftIDs(ctx context.Context, now int64) ([]int64, error) {
	var ids []int64
	err := s.db.WithContext(ctx).Model(&domain.Draft{}).
		Where("status = 'queued' OR (status = 'retry_wait' AND next_attempt_at <= ?)", now).
		Order("COALESCE(next_attempt_at, 0), id").Limit(100).Pluck("id", &ids).Error
	return ids, err
}

func (s *Store) RecoverSendingDrafts(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	message := "process stopped while SMTP delivery was in progress; delivery result is unknown"
	return s.db.WithContext(ctx).Model(&domain.Draft{}).Where("status = 'sending'").Updates(map[string]any{
		"status": "unknown", "last_error": message, "updated_at": time.Now().UnixMilli(),
	}).Error
}

func (s *Store) SetDraftDelivery(ctx context.Context, id int64, status string, attemptCount int, nextAttemptAt *int64, smtpCode *int, lastError *string, sentAt *int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Model(&domain.Draft{}).Where("id = ?", id).Updates(map[string]any{
		"status": status, "attempt_count": attemptCount, "next_attempt_at": nextAttemptAt,
		"last_smtp_code": smtpCode, "last_error": lastError, "sent_at": sentAt, "updated_at": time.Now().UnixMilli(),
	}).Error
}

func (s *Store) CreateSentMessage(ctx context.Context, message *domain.Message, draftID int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(message).Error; err != nil {
			var existing domain.Message
			if findErr := tx.Where("account_id = ? AND dedupe_key = ?", message.AccountID, message.DedupeKey).First(&existing).Error; findErr != nil {
				return err
			}
			message.ID = existing.ID
		}
		return tx.Model(&domain.Draft{}).Where("id = ?", draftID).Update("sent_message_id", message.ID).Error
	})
}

func HashBytes(value []byte) []byte {
	sum := sha256.Sum256(value)
	return sum[:]
}
