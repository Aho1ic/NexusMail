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

const (
	// maxBulkMessageIDs bounds one bulk mark-read so a stale view cannot queue an
	// unbounded IMAP STORE run against the provider.
	maxBulkMessageIDs = 2000
	// sqliteParameterChunk keeps IN (...) lists clear of SQLite's variable limit.
	sqliteParameterChunk = 500
)

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

// UpsertMailbox records what LIST reported about a mailbox: its name, delimiter,
// role and sync tier.
//
// It deliberately does not touch uid_next or highest_modseq. Those are the sync
// cursor and only UpdateMailboxCursor knows them; the caller here builds its
// struct from a LIST response, where both are always nil. Including them in the
// conflict clause meant every full sync wiped the cursor it had just written,
// which silently disabled both the cheap STATUS short-circuit that decides whether
// a mailbox needs syncing at all and any CONDSTORE narrowing that depends on a
// stored modseq.
func (s *Store) UpsertMailbox(ctx context.Context, mailbox *domain.Mailbox) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Exec(`INSERT INTO mailboxes
        (account_id, remote_name, display_name, delimiter, role, sync_mode, uid_validity, uid_next, highest_modseq, last_uid, last_sync_at, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(account_id, remote_name) DO UPDATE SET
        display_name=excluded.display_name, delimiter=excluded.delimiter, role=excluded.role,
        sync_mode=excluded.sync_mode, updated_at=excluded.updated_at`, mailbox.AccountID, mailbox.RemoteName, mailbox.DisplayName,
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

// feedColumns is the projection the message list returns. body_text and
// body_html are deliberately absent: a page of 40 messages would otherwise carry
// up to 40 full bodies, and the feed reloads on every realtime event — including
// the burst a first-sync body prefetch produces. The detail endpoint serves the
// body for the one message that is actually open.
const feedColumns = `messages.id, messages.account_id, messages.direction, messages.rfc_message_id,
	messages.provider_message_id, messages.in_reply_to, messages.references_json, messages.subject,
	messages.sender, messages.recipients, messages.from_json, messages.to_json, messages.cc_json,
	messages.bcc_json, messages.reply_to_json, messages.snippet, messages.body_state, messages.size_bytes,
	messages.sent_at, messages.received_at, messages.is_read, messages.is_starred, messages.has_attachments,
	messages.created_at, messages.updated_at`

func (s *Store) ListMessages(ctx context.Context, filter ports.MessageFilter) (ports.MessagePage, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	query := s.db.WithContext(ctx).Model(&domain.Message{}).Distinct(feedColumns)
	query = applyMessageScope(query, filter)
	if filter.IsRead != nil {
		query = query.Where("messages.is_read = ?", *filter.IsRead)
	}
	if filter.Query != "" {
		query = applyMessageSearch(query, filter.Query)
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
	total, err := s.unreadTotal(ctx, filter)
	if err != nil {
		return ports.MessagePage{}, err
	}
	page.UnreadTotal = total
	return page, nil
}

// unreadTotal counts the unread mail the whole view holds, ignoring the cursor:
// the count belongs to the view, not to the page being read.
func (s *Store) unreadTotal(ctx context.Context, filter ports.MessageFilter) (int, error) {
	query := applyMessageScope(s.db.WithContext(ctx).Model(&domain.Message{}), filter)
	if filter.Query != "" {
		query = applyMessageSearch(query, filter.Query)
	}
	var count int64
	err := query.Where("messages.is_read = 0 AND messages.direction = 'incoming'").
		Distinct("messages.id").Count(&count).Error
	return int(count), err
}

// applyMessageSearch applies the FTS or LIKE predicate shared by the message feed
// and any other query that filters on message text.
func applyMessageSearch(query *gorm.DB, value string) *gorm.DB {
	if utf8.RuneCountInString(value) >= 3 {
		return query.Joins("JOIN message_fts ON message_fts.rowid = messages.id").Where("message_fts MATCH ?", quoteFTS(value))
	}
	like := "%" + escapeLike(value) + "%"
	return query.Where("(messages.subject LIKE ? ESCAPE '\\' OR messages.sender LIKE ? ESCAPE '\\' OR messages.recipients LIKE ? ESCAPE '\\' OR messages.body_text LIKE ? ESCAPE '\\')", like, like, like, like)
}

// applyMessageScope adds the account, mailbox and folder predicates shared by the
// message feed and the bulk mark-read query, so the two can never disagree about
// which messages "the current view" contains.
func applyMessageScope(query *gorm.DB, filter ports.MessageFilter) *gorm.DB {
	if filter.AccountID != nil {
		query = query.Where("messages.account_id = ?", *filter.AccountID)
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
	return query
}

// UnreadMessageIDs lists the unread messages a view contains, newest first, up to
// limit. It does not reuse ListMessages because that clamps to a page of 100,
// while "mark everything read" has to reach the whole backlog. The cap is still
// bounded so one click cannot turn into an unbounded IMAP STORE run.
func (s *Store) UnreadMessageIDs(ctx context.Context, filter ports.MessageFilter, limit int) ([]int64, error) {
	if limit <= 0 || limit > maxBulkMessageIDs {
		limit = maxBulkMessageIDs
	}
	query := applyMessageScope(s.db.WithContext(ctx).Model(&domain.Message{}), filter)
	// The search term is part of the view: marking a filtered list read must not
	// reach the mail the filter is hiding.
	if filter.Query != "" {
		query = applyMessageSearch(query, filter.Query)
	}
	var ids []int64
	err := query.Where("messages.is_read = 0 AND messages.direction = 'incoming'").
		Distinct().Order("messages.received_at DESC, messages.id DESC").Limit(limit).
		Pluck("messages.id", &ids).Error
	return ids, err
}

// UpdateMessages applies one patch to many messages in a single transaction.
// writeMu is not re-entrant, so nothing here may call another locking write.
func (s *Store) UpdateMessages(ctx context.Context, ids []int64, patch ports.MessagePatch) error {
	if len(ids) == 0 {
		return nil
	}
	updates := map[string]any{"updated_at": time.Now().UnixMilli()}
	if patch.IsRead != nil {
		updates["is_read"] = *patch.IsRead
	}
	if patch.IsStarred != nil {
		updates["is_starred"] = *patch.IsStarred
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for start := 0; start < len(ids); start += sqliteParameterChunk {
			end := min(start+sqliteParameterChunk, len(ids))
			if err := tx.Model(&domain.Message{}).Where("id IN ?", ids[start:end]).Updates(updates).Error; err != nil {
				return err
			}
		}
		return nil
	})
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
