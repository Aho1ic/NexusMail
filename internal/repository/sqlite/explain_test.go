//go:build sqlite_fts5

package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/ports"

	"gorm.io/gorm"
)

// renderSQL returns the statement the given builder produces, with its bound
// arguments, without executing it. GORM's DryRun session fills Statement.SQL on
// Find/Count and skips the round trip.
//
// The point of going through feedQuery and unreadQuery rather than pasting SQL
// into the test is that a pasted copy stops describing the product the moment a
// predicate changes, and a query-plan test that checks the wrong query passes
// while the feed regresses.
func renderSQL(t *testing.T, store *Store, build func(*Store) *gorm.DB) (string, []any) {
	t.Helper()
	dry := &Store{db: store.db.Session(&gorm.Session{DryRun: true}), sqlDB: store.sqlDB, unreadCache: map[unreadCacheKey]unreadCacheEntry{}}
	query := build(dry)
	statement := query.Statement
	if statement.SQL.Len() == 0 {
		t.Fatalf("dry run produced no SQL")
	}
	return statement.SQL.String(), statement.Vars
}

func explainPlan(t *testing.T, store *Store, sql string, args []any) []string {
	t.Helper()
	rows, err := store.sqlDB.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+sql, args...)
	if err != nil {
		t.Fatalf("explain %q: %v", sql, err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(plan) == 0 {
		t.Fatalf("empty query plan for %q", sql)
	}
	return plan
}

// TestFeedQueryPlanUsesOrderedIndex is the regression test for the feed's plan.
//
// It asserts rather than logs. The previous version of this file printed the plan
// with t.Logf and asserted nothing, so the degradation it was written to watch for
// could not fail it: the default view — folder=inbox with no account, which
// App.tsx requests on first paint and after every realtime event — planned as
// SCAN mailbox_messages plus a temp B-tree for DISTINCT and a second one for
// ORDER BY, and the test stayed green while the query took 42ms on 100k messages.
//
// A temp B-tree is the specific failure: it means SQLite materialised and sorted
// the whole scoped set instead of walking (received_at DESC, id DESC) and stopping
// at LIMIT, so the cost grows with the mailbox rather than with the page.
func TestFeedQueryPlanUsesOrderedIndex(t *testing.T) {
	store := openTestStore(t)
	fixture := seedScopeFixture(t, store)
	unread := false

	cases := []struct {
		name   string
		filter ports.MessageFilter
	}{
		{"folder=inbox, every account (the default view)", ports.MessageFilter{Folder: "inbox"}},
		{"account + folder", ports.MessageFilter{AccountID: &fixture.account.ID, Folder: "inbox"}},
		{"account + folder + is_read", ports.MessageFilter{AccountID: &fixture.account.ID, Folder: "inbox", IsRead: &unread}},
		{"mailbox only", ports.MessageFilter{MailboxID: &fixture.inbox.ID}},
		{"account + mailbox", ports.MessageFilter{AccountID: &fixture.account.ID, MailboxID: &fixture.inbox.ID}},
		{"mailbox + folder", ports.MessageFilter{MailboxID: &fixture.inbox.ID, Folder: "inbox"}},
		{"unscoped", ports.MessageFilter{}},
		{"second page (cursor)", ports.MessageFilter{Folder: "inbox", Cursor: encodeCursor(cursorValue{ReceivedAt: time.Now().UnixMilli(), ID: 4})}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			sql, args := renderSQL(t, store, func(s *Store) *gorm.DB {
				query, err := s.feedQuery(context.Background(), testCase.filter, 40)
				if err != nil {
					t.Fatal(err)
				}
				var items []domain.Message
				return query.Find(&items)
			})
			plan := explainPlan(t, store, sql, args)
			joined := strings.Join(plan, "\n")
			for _, line := range plan {
				if strings.Contains(line, "TEMP B-TREE") {
					t.Errorf("plan materialises and sorts the whole set instead of walking the feed index:\n%s", joined)
					break
				}
			}
			if !mentionsFeedIndex(plan) {
				t.Errorf("messages is not reached through a (received_at, id) feed index:\n%s", joined)
			}
		})
	}
}

// The FTS branch is exempt from the ordered-index requirement: a MATCH is
// answered from the FTS index in rowid order, so SQLite has to sort the hits to
// produce received_at order however the scope is written. What must still hold is
// that the search does not additionally pay for DISTINCT, and that messages is
// reached by index rather than scanned.
func TestSearchQueryPlanScansOnlyTheFTSIndex(t *testing.T) {
	store := openTestStore(t)
	fixture := seedScopeFixture(t, store)
	sql, args := renderSQL(t, store, func(s *Store) *gorm.DB {
		query, err := s.feedQuery(context.Background(), ports.MessageFilter{AccountID: &fixture.account.ID, Folder: "inbox", Query: "hello"}, 40)
		if err != nil {
			t.Fatal(err)
		}
		var items []domain.Message
		return query.Find(&items)
	})
	plan := explainPlan(t, store, sql, args)
	joined := strings.Join(plan, "\n")
	for _, line := range plan {
		if strings.Contains(line, "TEMP B-TREE FOR DISTINCT") {
			t.Errorf("search plan still deduplicates with a temp B-tree:\n%s", joined)
		}
		// A bare "SCAN messages" with no index is the plan that reads the whole
		// table; the FTS virtual table is allowed to be scanned, that is what a
		// MATCH is.
		if strings.HasPrefix(line, "SCAN messages") && !strings.Contains(line, "INDEX") {
			t.Errorf("search plan scans the messages table:\n%s", joined)
		}
	}
}

// The unread badge runs on the same schedule as the feed, so its plan matters as
// much. It also no longer wraps the count in DISTINCT, which is only sound if the
// scope yields one row per message — the same property the temp-B-tree check
// happens to observe.
func TestUnreadQueryPlanUsesIndex(t *testing.T) {
	store := openTestStore(t)
	fixture := seedScopeFixture(t, store)
	for _, testCase := range []struct {
		name   string
		filter ports.MessageFilter
	}{
		{"folder=inbox, every account", ports.MessageFilter{Folder: "inbox"}},
		{"account + folder", ports.MessageFilter{AccountID: &fixture.account.ID, Folder: "inbox"}},
		{"account + mailbox", ports.MessageFilter{AccountID: &fixture.account.ID, MailboxID: &fixture.inbox.ID}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			sql, args := renderSQL(t, store, func(s *Store) *gorm.DB {
				var count int64
				return s.unreadQuery(context.Background(), testCase.filter).Count(&count)
			})
			plan := explainPlan(t, store, sql, args)
			joined := strings.Join(plan, "\n")
			for _, line := range plan {
				if strings.Contains(line, "TEMP B-TREE") {
					t.Errorf("unread count materialises the scoped set:\n%s", joined)
					break
				}
			}
			for _, line := range plan {
				if strings.HasPrefix(line, "SCAN mm") || strings.HasPrefix(line, "SCAN mailbox_messages") {
					t.Errorf("unread count drives from mailbox_messages instead of messages:\n%s", joined)
				}
			}
		})
	}
}

// mentionsFeedIndex reports whether the plan reaches messages through one of the
// indexes that carry (received_at DESC, id DESC), which is what lets LIMIT stop
// the walk early. SEARCH and SCAN are both acceptable — an ordered SCAN of the
// covering feed index is the ideal plan for the unscoped and folder-only views —
// but reaching the table without an index is not.
func mentionsFeedIndex(plan []string) bool {
	for _, line := range plan {
		if !strings.Contains(line, "messages") || strings.Contains(line, "mailbox_messages") || strings.Contains(line, "message_fts") {
			continue
		}
		if strings.Contains(line, "idx_messages_feed") || strings.Contains(line, "idx_messages_account_feed") ||
			strings.Contains(line, "idx_messages_account_read_feed") {
			return true
		}
	}
	return false
}
