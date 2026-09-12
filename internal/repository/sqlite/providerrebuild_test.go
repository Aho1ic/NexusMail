//go:build sqlite_fts5

package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/migrations"
)

// 000004 widens the provider CHECK, which on SQLite means rebuilding accounts —
// the parent of three ON DELETE CASCADE chains. With foreign keys enabled the
// DROP TABLE in that rebuild performs an implicit DELETE FROM that fires those
// cascades, so the naive version of this migration deletes every mailbox, message
// and draft in the database. Nothing reports it: the migration succeeds, the
// process starts, and the user's mail is simply gone.
//
// The guard is the foreign_keys=off directive the runner reads from line 1 of the
// script. This test drives the real runner over the real embedded migration, with
// children present, which is the only arrangement in which the mistake is visible.
func TestProviderRebuildKeepsChildRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	messageID := seedMessage(t, store, account.ID, mailbox.ID, 1, "before the rebuild", "incoming", false, time.Now().UnixMilli())
	now := time.Now().UnixMilli()
	draft := domain.Draft{
		AccountID: account.ID, RFCMessageID: "<rebuild@example.com>", Revision: 1,
		ToJSON: "[]", CCJSON: "[]", BCCJSON: "[]", Subject: "draft that must survive",
		Status: "draft", RemoteSyncState: "dirty", CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateDraft(ctx, &draft); err != nil {
		t.Fatal(err)
	}

	// Revert to the pre-000004 schema through the shipped down script, so the
	// fixture is the four-value CHECK an existing deployment actually has rather
	// than a hand-written approximation of it. Removing the bookkeeping row is what
	// makes the next Open re-apply the up migration.
	revert(t, store, 4)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatalf("re-applying 000004: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The account itself keeps its id, which is what the children's account_id
	// points at.
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].ID != account.ID {
		t.Fatalf("accounts after the rebuild = %#v, want the one seeded with id %d", accounts, account.ID)
	}
	for _, probe := range []struct {
		name  string
		query string
		want  int
	}{
		{"mailboxes", "SELECT count(*) FROM mailboxes", 1},
		{"messages", "SELECT count(*) FROM messages", 1},
		{"mailbox_messages", "SELECT count(*) FROM mailbox_messages", 1},
		{"drafts", "SELECT count(*) FROM drafts", 1},
	} {
		var got int
		if err := store.db.Raw(probe.query).Scan(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got != probe.want {
			t.Errorf("%s after the rebuild = %d, want %d: the cascade fired", probe.name, got, probe.want)
		}
	}
	// The message is still reachable through its mailbox mapping, so the rows are
	// not merely present but still joined to the account that was rebuilt.
	stored, _, err := store.GetMessage(ctx, messageID)
	if err != nil {
		t.Fatalf("get message %d: %v", messageID, err)
	}
	if stored.AccountID != account.ID {
		t.Errorf("message account_id = %d, want %d", stored.AccountID, account.ID)
	}
}

// The widened constraint has to actually admit the two new providers, and still
// refuse anything outside the set — a rebuild that dropped the CHECK altogether
// would pass the survival test above while silently accepting any string.
func TestProviderRebuildAdmitsExactlyTheKnownProviders(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for _, name := range []string{"qq", "163", "126", "gmail", "outlook", "icloud"} {
		account := providerAccount(name, name+"@example.com")
		if err := store.CreateAccount(ctx, &account); err != nil {
			t.Errorf("provider %q was refused by the CHECK: %v", name, err)
		}
	}
	unknown := providerAccount("fastmail", "someone@fastmail.com")
	if err := store.CreateAccount(ctx, &unknown); err == nil {
		t.Error("an unknown provider was stored: the rebuild dropped the CHECK instead of widening it")
	}
}

// Cascades must still work after the rebuild. The migration runs with foreign keys
// off, and a rebuild that restated the columns but not the UNIQUE index or left
// the children unreferenced would leave deletes silently orphaning rows instead of
// removing them.
func TestProviderRebuildLeavesCascadesIntact(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, mailbox := seedAccountMailbox(t, store)
	seedMessage(t, store, account.ID, mailbox.ID, 1, "cascade fodder", "incoming", false, time.Now().UnixMilli())
	if err := store.DeleteAccount(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	for _, probe := range []string{"mailboxes", "messages", "mailbox_messages"} {
		var got int
		if err := store.db.Raw("SELECT count(*) FROM " + probe).Scan(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got != 0 {
			t.Errorf("%s after deleting the account = %d, want 0: the cascade is gone", probe, got)
		}
	}
	// And the UNIQUE (provider, email) index the rebuild had to restate by hand.
	first := providerAccount("126", "dup@126.com")
	if err := store.CreateAccount(ctx, &first); err != nil {
		t.Fatal(err)
	}
	second := providerAccount("126", "dup@126.com")
	if err := store.CreateAccount(ctx, &second); err == nil {
		t.Error("the same address was stored twice for one provider: UNIQUE (provider, email) was not restored")
	}
}

// The runner disables foreign keys on one pinned connection. If that connection is
// handed back to the pool without the pragma restored, every cascade that later
// runs on it silently does nothing — the failure surfaces much later as orphaned
// rows, with no connection between cause and effect.
//
// TestOpenAppliesTheConfiguredPragmas samples a single connection and cannot see
// this: the pool holds up to 8, and only one of them was ever touched. The probes
// here are held open concurrently so each one occupies a distinct connection.
func TestForeignKeysSurviveOnEveryPooledConnection(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	const probes = 8

	var start, done sync.WaitGroup
	start.Add(probes)
	done.Add(probes)
	results := make([]string, probes)
	release := make(chan struct{})
	for index := range probes {
		go func() {
			defer done.Done()
			conn, err := store.sqlDB.Conn(ctx)
			if err != nil {
				results[index] = "conn: " + err.Error()
				start.Done()
				return
			}
			defer func() { _ = conn.Close() }()
			if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&results[index]); err != nil {
				results[index] = "pragma: " + err.Error()
			}
			// Hold the connection until every goroutine has one, so no two probes
			// can be answered by the same connection.
			start.Done()
			<-release
		}()
	}
	start.Wait()
	close(release)
	done.Wait()

	for index, got := range results {
		if got != "1" {
			t.Errorf("connection %d has PRAGMA foreign_keys=%q, want \"1\"", index, got)
		}
	}
}

// A migration that leaves a child pointing at a parent row that no longer exists
// must fail the migration rather than be discovered later as short reads. The
// runner's post-commit foreign_key_check is what catches it; this drives that
// check directly, since no shipped migration produces the condition.
func TestCheckForeignKeysReportsOrphans(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	account, _ := seedAccountMailbox(t, store)

	conn, err := store.sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := store.checkForeignKeys(ctx, conn, "clean.up.sql"); err != nil {
		t.Fatalf("a consistent database was reported as broken: %v", err)
	}
	// Orphan the mailbox the way a rebuild that lost the parent ids would: with
	// foreign keys off, so the delete is not itself refused or cascaded.
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM accounts WHERE id=?", account.ID); err != nil {
		t.Fatal(err)
	}
	err = store.checkForeignKeys(ctx, conn, "broken.up.sql")
	if err == nil {
		t.Fatal("orphaned rows were not reported")
	}
	if !strings.Contains(err.Error(), "mailboxes") || !strings.Contains(err.Error(), "broken.up.sql") {
		t.Errorf("error = %q, want it to name the table and the migration", err)
	}
}

// revert applies a migration's down script and forgets its bookkeeping row, so the
// next Open re-applies the up script. It goes through applyMigration so the down
// script runs under the same directive handling as the up one — a down script that
// needs foreign keys off and does not get it destroys the fixture this helper
// exists to build.
func revert(t *testing.T, store *Store, version int) {
	t.Helper()
	name := ""
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		parsed, ok := parseMigrationName(entry.Name())
		if ok && parsed == version && strings.HasSuffix(entry.Name(), ".down.sql") {
			name = entry.Name()
		}
	}
	if name == "" {
		t.Fatalf("no down migration for version %d", version)
	}
	ctx := context.Background()
	// The bookkeeping row goes first: applyMigration records the version it ran, and
	// the down script is recorded under the same one. Dropping it up front both
	// clears that collision and is what makes the next Open re-apply the up script.
	if _, err := store.sqlDB.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version=?", version); err != nil {
		t.Fatal(err)
	}
	if err := store.applyMigration(ctx, version, name); err != nil {
		t.Fatalf("apply %s: %v", name, err)
	}
	if _, err := store.sqlDB.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version=?", version); err != nil {
		t.Fatal(err)
	}
}

func providerAccount(provider, email string) domain.Account {
	now := time.Now().UnixMilli()
	return domain.Account{
		Email: email, DisplayName: provider, Provider: provider, AuthType: "password", Username: email,
		IMAPHost: "imap.example.com", IMAPPort: 993, IMAPTLSMode: "implicit",
		SMTPHost: "smtp.example.com", SMTPPort: 465, SMTPTLSMode: "implicit",
		SecretCiphertext: []byte("sealed"), Status: "disconnected", CreatedAt: now, UpdatedAt: now,
	}
}
