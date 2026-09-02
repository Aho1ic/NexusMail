//go:build sqlite_fts5

package sqlite

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Open is the first thing main.go calls, so its failures are startup failures. The
// contract that matters is that a database it could not fully prepare is never
// returned: main.go has no way to tell a half-open store from a good one, and
// every later query would fail one at a time instead of the process refusing to
// start. The happy path was well covered by every other test in this package;
// the refusals were not.

// TestOpenRefusesAPathWhoseParentIsAFile covers the ensureParent failure. A
// NEXUSMAIL_DB_PATH pointing under an existing file is a plausible misconfiguration
// — a trailing path segment appended to a file by mistake — and MkdirAll cannot
// create a directory there.
func TestOpenRefusesAPathWhoseParentIsAFile(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Open(filepath.Join(blocker, "mail.db"))
	if err == nil {
		_ = store.Close()
		t.Fatal("Open succeeded with a file where the parent directory should be")
	}
	if store != nil {
		t.Error("Open returned a store alongside its error; main.go cannot tell it apart from a working one")
	}
}

// TestOpenRefusesAnUnusableDatabaseFile covers the failure after the directory
// exists: the path is a directory, so SQLite cannot open it as a database. This is
// the branch that must not leave a handle behind — Open closes the pool on every
// post-open failure, and a leaked one would hold the file for the process's life.
func TestOpenRefusesAnUnusableDatabaseFile(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "mail.db")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}

	store, err := Open(directory)
	if err == nil {
		_ = store.Close()
		t.Fatal("Open succeeded on a directory")
	}
	if store != nil {
		t.Error("Open returned a store alongside its error")
	}
}

// TestOpenCreatesMissingParentDirectories is the positive half of ensureParent: a
// first run on a fresh volume names a database under a directory that does not
// exist yet, and Open is what creates it. Without this the first start of a new
// deployment fails.
func TestOpenCreatesMissingParentDirectories(t *testing.T) {
	nested := filepath.Join(t.TempDir(), "data", "sqlite", "mail.db")

	store, err := Open(nested)
	if err != nil {
		t.Fatalf("Open on a missing parent: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := os.Stat(nested); err != nil {
		t.Errorf("database file was not created: %v", err)
	}
	// A store that opened must be migrated, since Open is the only place that runs
	// migrations and callers assume the schema is present.
	if versions := recordedVersions(t, store); len(versions) == 0 {
		t.Error("Open returned a store with no migrations applied")
	}
}

// TestOpenAppliesTheConfiguredPragmas pins the settings the rest of the package
// depends on. WAL plus a busy timeout is what lets the single writeMu serialize
// writes without SQLITE_BUSY on the 8-connection pool, and foreign keys are what
// makes the mailbox_messages cleanup in DeleteMailboxUIDs reliable. Each is set in
// the DSN and again as a PRAGMA, so a silent drop of either would not show up
// until a concurrent write failed.
func TestOpenAppliesTheConfiguredPragmas(t *testing.T) {
	store := openTestStore(t)

	for _, probe := range []struct {
		pragma string
		want   string
	}{
		{"PRAGMA journal_mode", "wal"},
		{"PRAGMA foreign_keys", "1"},
		{"PRAGMA busy_timeout", "5000"},
	} {
		var got string
		if err := store.db.Raw(probe.pragma).Scan(&got).Error; err != nil {
			t.Fatalf("%s: %v", probe.pragma, err)
		}
		if !strings.EqualFold(got, probe.want) {
			t.Errorf("%s = %q, want %q", probe.pragma, got, probe.want)
		}
	}
}
