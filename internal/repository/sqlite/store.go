package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"
	"nexusmail/migrations"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var ErrConflict = errors.New("revision conflict")

type Store struct {
	db      *gorm.DB
	sqlDB   *sql.DB
	writeMu sync.Mutex
}

func Open(path string) (*Store, error) {
	if err := ensureParent(path); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL&_synchronous=NORMAL", path)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get database handle: %w", err)
	}
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(4)
	sqlDB.SetConnMaxLifetime(0)
	store := &Store{db: db, sqlDB: sqlDB}
	if err := store.configure(context.Background()); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if err := store.migrate(context.Background()); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return store, nil
}

func ensureParent(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o750)
}

func (s *Store) configure(ctx context.Context) error {
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL", "PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000", "PRAGMA wal_autocheckpoint=1000",
	} {
		if _, err := s.sqlDB.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("configure sqlite (%s): %w", pragma, err)
		}
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	var count int
	if err := s.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'").Scan(&count); err != nil {
		return fmt.Errorf("inspect migrations: %w", err)
	}
	if count > 0 {
		var applied int
		if err := s.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE version=1").Scan(&applied); err != nil {
			return fmt.Errorf("read migration version: %w", err)
		}
		if applied > 0 {
			return nil
		}
	}
	script, err := migrations.FS.ReadFile("000001_init.up.sql")
	if err != nil {
		return fmt.Errorf("read migration: %w", err)
	}
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	if _, err = tx.ExecContext(ctx, string(script)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply migration (build with -tags sqlite_fts5): %w", err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES(1, ?)", time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record migration: %w", err)
	}
	return tx.Commit()
}

func (s *Store) Ping(ctx context.Context) error { return s.sqlDB.PingContext(ctx) }
func (s *Store) Close() error                   { return s.sqlDB.Close() }

func (s *Store) CreateAccount(ctx context.Context, account *domain.Account) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Create(account).Error
}

func (s *Store) GetAccount(ctx context.Context, id int64) (domain.Account, error) {
	var account domain.Account
	err := s.db.WithContext(ctx).First(&account, id).Error
	return account, err
}

func (s *Store) ListAccounts(ctx context.Context) ([]domain.Account, error) {
	var accounts []domain.Account
	err := s.db.WithContext(ctx).Order("id ASC").Find(&accounts).Error
	return accounts, err
}

func (s *Store) UpdateAccountStatus(ctx context.Context, id int64, status string, lastError *string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	values := map[string]any{"status": status, "last_error": lastError, "updated_at": time.Now().UnixMilli()}
	if status == "connected" {
		values["last_connected_at"] = time.Now().UnixMilli()
	}
	return s.db.WithContext(ctx).Model(&domain.Account{}).Where("id = ?", id).Updates(values).Error
}

func (s *Store) UpsertMailbox(ctx context.Context, mailbox *domain.Mailbox) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Exec(`INSERT INTO mailboxes
        (account_id, remote_name, display_name, delimiter, role, sync_mode, uid_validity, uid_next, highest_modseq, last_uid, last_sync_at, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(account_id, remote_name) DO UPDATE SET
        display_name=excluded.display_name, delimiter=excluded.delimiter, role=excluded.role,
        sync_mode=excluded.sync_mode, uid_next=excluded.uid_next, highest_modseq=excluded.highest_modseq,
        updated_at=excluded.updated_at`, mailbox.AccountID, mailbox.RemoteName, mailbox.DisplayName,
		mailbox.Delimiter, mailbox.Role, mailbox.SyncMode, mailbox.UIDValidity, mailbox.UIDNext,
		mailbox.HighestModSeq, mailbox.LastUID, mailbox.LastSyncAt, mailbox.CreatedAt, mailbox.UpdatedAt).Error
}

func (s *Store) ListMailboxes(ctx context.Context, accountID int64) ([]domain.Mailbox, error) {
	var items []domain.Mailbox
	err := s.db.WithContext(ctx).Where("account_id = ?", accountID).Order("CASE role WHEN 'inbox' THEN 0 WHEN 'sent' THEN 1 WHEN 'drafts' THEN 2 WHEN 'archive' THEN 3 ELSE 9 END, display_name").Find(&items).Error
	return items, err
}

func (s *Store) CreateOrUpdateMessage(ctx context.Context, message *domain.Message, mailboxID int64, uid uint32, flags []string, internalDate time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	created := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing domain.Message
		err := tx.Where("account_id = ? AND dedupe_key = ?", message.AccountID, message.DedupeKey).First(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if err := tx.Create(message).Error; err != nil {
				return err
			}
			created = true
		} else if err != nil {
			return err
		} else {
			message.ID = existing.ID
			if err := tx.Model(&existing).Updates(map[string]any{
				"subject": message.Subject, "sender": message.Sender, "recipients": message.Recipients,
				"from_json": message.FromJSON, "to_json": message.ToJSON, "cc_json": message.CCJSON,
				"is_read": message.IsRead, "is_starred": message.IsStarred, "updated_at": message.UpdatedAt,
			}).Error; err != nil {
				return err
			}
		}
		flagsJSON, _ := json.Marshal(flags)
		return tx.Exec(`INSERT INTO mailbox_messages(mailbox_id, message_id, uid, flags_json, internal_date)
            VALUES (?, ?, ?, ?, ?) ON CONFLICT(mailbox_id, uid) DO UPDATE SET
            message_id=excluded.message_id, flags_json=excluded.flags_json, internal_date=excluded.internal_date`,
			mailboxID, message.ID, uid, string(flagsJSON), internalDate.UnixMilli()).Error
	})
	return created, err
}

type cursorValue struct {
	ReceivedAt int64 `json:"r"`
	ID         int64 `json:"i"`
}

func (s *Store) ListMessages(ctx context.Context, filter ports.MessageFilter) (ports.MessagePage, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	query := s.db.WithContext(ctx).Model(&domain.Message{}).Distinct("messages.*")
	if filter.AccountID != nil {
		query = query.Where("messages.account_id = ?", *filter.AccountID)
	}
	if filter.IsRead != nil {
		query = query.Where("messages.is_read = ?", *filter.IsRead)
	}
	if filter.MailboxID != nil || filter.Folder != "" {
		query = query.Joins("JOIN mailbox_messages mm ON mm.message_id = messages.id").Joins("JOIN mailboxes mb ON mb.id = mm.mailbox_id")
		if filter.MailboxID != nil {
			query = query.Where("mb.id = ?", *filter.MailboxID)
		}
		if filter.Folder != "" {
			query = query.Where("mb.role = ?", filter.Folder)
		}
	}
	if filter.Query != "" {
		if utf8.RuneCountInString(filter.Query) >= 3 {
			query = query.Joins("JOIN message_fts ON message_fts.rowid = messages.id").Where("message_fts MATCH ?", quoteFTS(filter.Query))
		} else {
			like := "%" + escapeLike(filter.Query) + "%"
			query = query.Where("(messages.subject LIKE ? ESCAPE '\\' OR messages.sender LIKE ? ESCAPE '\\' OR messages.recipients LIKE ? ESCAPE '\\' OR messages.body_text LIKE ? ESCAPE '\\')", like, like, like, like)
		}
	}
	if filter.Cursor != "" {
		cursor, err := decodeCursor(filter.Cursor)
		if err != nil {
			return ports.MessagePage{}, err
		}
		query = query.Where("(messages.received_at < ? OR (messages.received_at = ? AND messages.id < ?))", cursor.ReceivedAt, cursor.ReceivedAt, cursor.ID)
	}
	var items []domain.Message
	if err := query.Order("messages.received_at DESC, messages.id DESC").Limit(limit + 1).Find(&items).Error; err != nil {
		return ports.MessagePage{}, err
	}
	page := ports.MessagePage{Items: items}
	if len(items) > limit {
		last := items[limit-1]
		page.Items = items[:limit]
		page.NextCursor = encodeCursor(cursorValue{ReceivedAt: last.ReceivedAt, ID: last.ID})
	}
	return page, nil
}

func (s *Store) GetMessage(ctx context.Context, id int64) (domain.Message, []domain.Attachment, error) {
	var message domain.Message
	if err := s.db.WithContext(ctx).First(&message, id).Error; err != nil {
		return message, nil, err
	}
	var attachments []domain.Attachment
	err := s.db.WithContext(ctx).Where("message_id = ?", id).Order("id").Find(&attachments).Error
	return message, attachments, err
}

func (s *Store) UpdateMessage(ctx context.Context, id int64, patch ports.MessagePatch) (domain.Message, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	updates := map[string]any{"updated_at": time.Now().UnixMilli()}
	if patch.IsRead != nil {
		updates["is_read"] = *patch.IsRead
	}
	if patch.IsStarred != nil {
		updates["is_starred"] = *patch.IsStarred
	}
	if err := s.db.WithContext(ctx).Model(&domain.Message{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return domain.Message{}, err
	}
	return s.GetMessageOnly(ctx, id)
}

func (s *Store) GetMessageOnly(ctx context.Context, id int64) (domain.Message, error) {
	var message domain.Message
	err := s.db.WithContext(ctx).First(&message, id).Error
	return message, err
}

func quoteFTS(input string) string { return `"` + strings.ReplaceAll(input, `"`, `""`) + `"` }
func escapeLike(input string) string {
	input = strings.ReplaceAll(input, `\`, `\\`)
	input = strings.ReplaceAll(input, `%`, `\%`)
	return strings.ReplaceAll(input, `_`, `\_`)
}
func encodeCursor(value cursorValue) string {
	b, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeCursor(value string) (cursorValue, error) {
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursorValue{}, errors.New("invalid cursor")
	}
	var cursor cursorValue
	if err := json.Unmarshal(b, &cursor); err != nil || cursor.ID <= 0 {
		return cursorValue{}, errors.New("invalid cursor")
	}
	return cursor, nil
}
