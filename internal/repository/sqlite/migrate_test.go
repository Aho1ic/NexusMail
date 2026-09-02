//go:build sqlite_fts5

package sqlite

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"nexusmail/migrations"
)

// The version prefix is the only ordering signal the runner has, and a name it
// cannot parse is skipped without a word: the process starts, the schema is one
// migration behind, and the first query against the missing column is where it
// surfaces. This test is the guard for the files that actually ship — a new
// migration added as "add_index.up.sql" fails here instead of in production.
func TestEveryShippedMigrationParses(t *testing.T) {
	files, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]string{}
	found := 0
	for _, file := range files {
		name := file.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		found++
		version, ok := parseMigrationName(name)
		if !ok {
			t.Errorf("migration %q has no parsable version prefix, so the runner skips it in silence", name)
			continue
		}
		if other, clash := seen[version]; clash {
			t.Errorf("migrations %q and %q share version %d, so one silently replaces the other", other, name, version)
		}
		seen[version] = name
	}
	if found == 0 {
		t.Fatal("no .up.sql migrations were embedded, so this test proves nothing")
	}
}

// The rejection reasons, each of which produces the same silent skip. They are
// pinned separately because the runner cannot distinguish them and neither can a
// log line: the only way to know which rule a filename broke is this table.
func TestParseMigrationNameRejects(t *testing.T) {
	for name, input := range map[string]string{
		"no underscore":         "000002.up.sql",
		"empty prefix":          "_fts5.up.sql",
		"non-numeric prefix":    "initial_schema.up.sql",
		"prefix with a space":   " 2_schema.up.sql",
		"zero version":          "000000_schema.up.sql",
		"negative version":      "-1_schema.up.sql",
		"empty name":            "",
		"underscore only":       "_",
		"decimal prefix":        "1.5_schema.up.sql",
		"hex prefix":            "0x2_schema.up.sql",
		"trailing underscore":   "2",
		"numeric after the cut": "v2_schema.up.sql",
	} {
		t.Run(name, func(t *testing.T) {
			if version, ok := parseMigrationName(input); ok {
				t.Errorf("%q parsed as version %d; the runner would order it by a number the filename does not state", input, version)
			}
		})
	}
}

// Accepted forms, including the zero padding the shipped files use and a version
// with no padding at all, since the prefix is read as an integer and not as text.
//
// "+2_" is here rather than in the rejection table because strconv.Atoi accepts a
// leading sign, so it parses as 2 and would collide with 000002. That is worth
// knowing but not worth a rule of its own: the collision it could cause is what
// TestEveryShippedMigration checks, against the files that actually ship.
func TestParseMigrationNameAccepts(t *testing.T) {
	for input, want := range map[string]int{
		"000001_initial.up.sql":     1,
		"000002_fts5_unicode61.sql": 2,
		"2_short.up.sql":            2,
		"10_ten.up.sql":             10,
		"000010_ten.up.sql":         10,
		"7_label_with_underscores":  7,
		"+2_signed.up.sql":          2,
	} {
		version, ok := parseMigrationName(input)
		if !ok {
			t.Errorf("%q was rejected", input)
			continue
		}
		if version != want {
			t.Errorf("%q parsed as %d, want %d", input, version, want)
		}
	}
}

// Padding must not change the order, which is the reason the prefix is compared as
// an integer: sorted as text, "10" precedes "2" and a later migration runs first.
func TestMigrationVersionsOrderNumericallyNotLexically(t *testing.T) {
	names := []string{"10_later.up.sql", "2_earlier.up.sql"}
	versions := make([]int, 0, len(names))
	for _, name := range names {
		version, ok := parseMigrationName(name)
		if !ok {
			t.Fatalf("%q was rejected", name)
		}
		versions = append(versions, version)
	}
	sort.Ints(versions)
	if versions[0] != 2 || versions[1] != 10 {
		t.Fatalf("sorted versions = %v, want [2 10]", versions)
	}
}

// Opening the same file twice must apply nothing the second time. Migrations are not
// all idempotent on their own — a CREATE TABLE without IF NOT EXISTS fails on the
// second pass — so schema_migrations is what makes a restart safe, and a bookkeeping
// row that does not land turns every restart into a failed migration.
func TestReopeningAppliesNothingTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	applied := recordedVersions(t, first)
	if len(applied) == 0 {
		t.Fatal("opening a fresh database recorded no migrations")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopening a migrated database failed, so a restart would not come up: %v", err)
	}
	defer second.Close()
	if again := recordedVersions(t, second); len(again) != len(applied) {
		t.Errorf("schema_migrations holds %d versions after reopening, want %d: a migration ran twice or recorded twice", len(again), len(applied))
	}
}

func recordedVersions(t *testing.T, store *Store) []int {
	t.Helper()
	rows, err := store.sqlDB.QueryContext(context.Background(), "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var versions []int
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return versions
}
