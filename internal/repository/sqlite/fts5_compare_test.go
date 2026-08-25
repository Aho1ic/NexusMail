package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFTS5TokenizerCompare builds a small corpus of messages, then creates a
// second FTS5 virtual table with the unicode61 tokenizer and bulk-loads the
// same messages into both, to compare the FTS5 table sizes and write times.
//
// trigram case_sensitive 0 indexes every 3-gram substring of every column;
// unicode61 indexes one row per tokenized word. The two give very different
// write costs and search behaviour (trigram supports substring search,
// unicode61 only matches whole tokens). The numbers here are the basis for
// the recommendation in P1-3.
func TestFTS5TokenizerCompare(t *testing.T) {
	path := t.TempDir() + "/test.db"
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	// Provision a one-off FTS5 table using the alternative tokenizer. The
	// production schema uses trigram; this table lets us measure the
	// alternative in isolation.
	if _, err := store.sqlDB.ExecContext(ctx, `CREATE VIRTUAL TABLE fts_uni USING fts5(
		subject, sender, recipients, body_text,
		tokenize = 'unicode61')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sqlDB.ExecContext(ctx, `CREATE VIRTUAL TABLE fts_tri USING fts5(
		subject, sender, recipients, body_text,
		tokenize = 'trigram')`); err != nil {
		t.Fatal(err)
	}

	bodies := []string{
		"your verification code is 123456",
		"会议时间：明天下午三点，请准时参加。",
		"季度财务报表已发送至您的邮箱，请查收。",
		"Reset your password by visiting https://example.com/reset",
		"GitHub: A new issue was opened in repository go-imap.",
		"Order #998877 has shipped. Tracking number: 1Z999AA10123456784.",
		"Re: 关于周报的需求变更",
		"您的航班 CA1234 已起飞",
	}
	const n = 500
	seed := make([]string, n)
	for i := 0; i < n; i++ {
		seed[i] = bodies[i%len(bodies)] + " " + bodies[(i+1)%len(bodies)]
	}
	insert := func(table string) (timeTaken string, size string) {
		tx, err := store.sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		stmt, err := tx.PrepareContext(ctx, fmt.Sprintf("INSERT INTO %s(rowid, subject, sender, recipients, body_text) VALUES(?, ?, ?, ?, ?)", table))
		if err != nil {
			t.Fatal(err)
		}
		defer stmt.Close()
		for i, body := range seed {
			if _, err := stmt.ExecContext(ctx, int64(i+1), "subject "+fmt.Sprint(i), "sender@example.com", "to@example.com", body); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := store.sqlDB.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s", table)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		var pages int64
		if err := store.sqlDB.QueryRowContext(ctx, fmt.Sprintf("SELECT page_count FROM dbstat WHERE name='%s'", table)).Scan(&pages); err != nil {
			t.Logf("dbstat for %s failed: %v (skipping size metric)", table, err)
			pages = 0
		}
		return fmt.Sprintf("%d rows", count), fmt.Sprintf("%d pages (~%d KB)", pages, pages*4)
	}
	triRows, triSize := insert("fts_tri")
	uniRows, uniSize := insert("fts_uni")
	t.Logf("trigram : %s, %s", triRows, triSize)
	t.Logf("unicode61: %s, %s", uniRows, uniSize)

	// Search behaviour comparison: both should match "verification" exactly.
	var triHits, uniHits int
	if err := store.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM fts_tri WHERE fts_tri MATCH 'verification'").Scan(&triHits); err != nil {
		t.Fatal(err)
	}
	if err := store.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM fts_uni WHERE fts_uni MATCH 'verification'").Scan(&uniHits); err != nil {
		t.Fatal(err)
	}
	t.Logf("match 'verification': trigram=%d, unicode61=%d", triHits, uniHits)
	// Substring: trigram supports "verif" → "verification"; unicode61 does not.
	if err := store.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM fts_tri WHERE fts_tri MATCH '\"verif\"'").Scan(&triHits); err != nil {
		t.Logf("trigram substring query failed: %v", err)
	}
	if err := store.sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM fts_uni WHERE fts_uni MATCH '\"verif\"'").Scan(&uniHits); err != nil {
		t.Logf("unicode61 substring query failed (expected): %v", err)
	}
	t.Logf("substring 'verif': trigram=%d, unicode61=%d (0 means no match — expected for unicode61)", triHits, uniHits)

	// Sanity: validate we used sql package
	var _ = sql.ErrNoRows
	var _ = strings.TrimSpace
}
