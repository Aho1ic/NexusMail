package sqlite

import (
	"context"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"

	"gorm.io/gorm"
)

func (s *Store) GetMailboxByRole(ctx context.Context, accountID int64, role string) (domain.Mailbox, error) {
	var mailbox domain.Mailbox
	err := s.db.WithContext(ctx).Where("account_id = ? AND role = ?", accountID, role).Order("id").First(&mailbox).Error
	return mailbox, err
}

func (s *Store) GetMailbox(ctx context.Context, id int64) (domain.Mailbox, error) {
	var mailbox domain.Mailbox
	err := s.db.WithContext(ctx).First(&mailbox, id).Error
	return mailbox, err
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
		return location, err
	}
	if err = s.db.WithContext(ctx).First(&location.Account, mapping.AccountID).Error; err != nil {
		return location, err
	}
	if err = s.db.WithContext(ctx).First(&location.Mailbox, mapping.MailboxID).Error; err != nil {
		return location, err
	}
	location.UID = mapping.UID
	return location, nil
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

func (s *Store) UpsertAttachment(ctx context.Context, attachment *domain.Attachment) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Exec(`INSERT INTO attachments
        (message_id, part_id, filename, content_type, disposition, content_id, size_bytes, fetch_state, blob_id, last_error, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(message_id, part_id) DO UPDATE SET filename=excluded.filename, content_type=excluded.content_type,
        disposition=excluded.disposition, content_id=excluded.content_id, size_bytes=excluded.size_bytes, updated_at=excluded.updated_at`,
		attachment.MessageID, attachment.PartID, attachment.Filename, attachment.ContentType, attachment.Disposition,
		attachment.ContentID, attachment.SizeBytes, attachment.FetchState, attachment.BlobID, attachment.LastError,
		attachment.CreatedAt, attachment.UpdatedAt).Error
}

func (s *Store) GetAttachment(ctx context.Context, messageID, attachmentID int64) (domain.Attachment, error) {
	var attachment domain.Attachment
	err := s.db.WithContext(ctx).Where("id = ? AND message_id = ?", attachmentID, messageID).First(&attachment).Error
	return attachment, err
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
