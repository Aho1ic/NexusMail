package sqlite

import (
	"context"
	"testing"
	"time"

	"nexusmail/internal/domain"
)

func TestExplainListMessages(t *testing.T) {
	path := t.TempDir() + "/test.db"
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	acc := &domain.Account{Email: "x@x", DisplayName: "x", Provider: "qq", AuthType: "password", Username: "x@x", IMAPHost: "h", IMAPPort: 993, IMAPTLSMode: "implicit", SMTPHost: "h", SMTPPort: 465, SMTPTLSMode: "implicit", SecretCiphertext: []byte("k"), Status: "disconnected", CreatedAt: now, UpdatedAt: now}
	if err := store.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	mb := &domain.Mailbox{AccountID: acc.ID, RemoteName: "INBOX", DisplayName: "INBOX", Role: "inbox", SyncMode: "realtime", UIDValidity: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertMailbox(ctx, mb); err != nil {
		t.Fatal(err)
	}

	// Mimic what GORM produces for ListMessages. The feedColumns projection is
	// fixed; the WHERE shape is what we want to check.
	queries := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "folder=inbox only (all accounts)",
			sql: `EXPLAIN QUERY PLAN
				SELECT DISTINCT messages.id, messages.received_at FROM messages
				JOIN mailbox_messages mm ON mm.message_id = messages.id
				JOIN mailboxes mb ON mb.id = mm.mailbox_id
				WHERE mb.role = 'inbox'
				ORDER BY messages.received_at DESC, messages.id DESC
				LIMIT 41`,
		},
		{
			name: "account_id + folder=inbox (no read filter)",
			sql: `EXPLAIN QUERY PLAN
				SELECT DISTINCT messages.id, messages.received_at FROM messages
				JOIN mailbox_messages mm ON mm.message_id = messages.id
				JOIN mailboxes mb ON mb.id = mm.mailbox_id
				WHERE messages.account_id = ? AND mb.role = 'inbox'
				ORDER BY messages.received_at DESC, messages.id DESC
				LIMIT 41`,
			args: []any{acc.ID},
		},
		{
			name: "account_id + folder=inbox + is_read=0",
			sql: `EXPLAIN QUERY PLAN
				SELECT DISTINCT messages.id, messages.received_at FROM messages
				JOIN mailbox_messages mm ON mm.message_id = messages.id
				JOIN mailboxes mb ON mb.id = mm.mailbox_id
				WHERE messages.account_id = ? AND mb.role = 'inbox' AND messages.is_read = 0
				ORDER BY messages.received_at DESC, messages.id DESC
				LIMIT 41`,
			args: []any{acc.ID},
		},
		{
			name: "unread total path",
			sql: `EXPLAIN QUERY PLAN
				SELECT DISTINCT messages.id FROM messages
				JOIN mailbox_messages mm ON mm.message_id = messages.id
				JOIN mailboxes mb ON mb.id = mm.mailbox_id
				WHERE messages.account_id = ? AND mb.role = 'inbox' AND messages.is_read = 0 AND messages.direction = 'incoming'`,
			args: []any{acc.ID},
		},
		{
			name: "mailbox_id (single mailbox)",
			sql: `EXPLAIN QUERY PLAN
				SELECT DISTINCT messages.id, messages.received_at FROM messages
				JOIN mailbox_messages mm ON mm.message_id = messages.id
				JOIN mailboxes mb ON mb.id = mm.mailbox_id
				WHERE mb.id = ?
				ORDER BY messages.received_at DESC, messages.id DESC
				LIMIT 41`,
			args: []any{mb.ID},
		},
		{
			name: "FTS search (>=3 chars)",
			sql: `EXPLAIN QUERY PLAN
				SELECT DISTINCT messages.id, messages.received_at FROM messages
				JOIN message_fts ON message_fts.rowid = messages.id
				JOIN mailbox_messages mm ON mm.message_id = messages.id
				JOIN mailboxes mb ON mb.id = mm.mailbox_id
				WHERE messages.account_id = ? AND mb.role = 'inbox' AND message_fts MATCH 'hello'
				ORDER BY messages.received_at DESC, messages.id DESC
				LIMIT 41`,
			args: []any{acc.ID},
		},
	}
	for _, q := range queries {
		t.Run(q.name, func(t *testing.T) {
			rows, err := store.sqlDB.QueryContext(ctx, q.sql, q.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
					t.Fatal(err)
				}
				t.Logf("%s", detail)
			}
		})
	}
}
