//go:build sqlite_fts5

package sqlite

import (
	"context"
	"strings"
	"testing"

	"nexusmail/internal/ports"
)

// ftsPrefix is the one function that turns a user-supplied search string into an
// FTS5 expression, so it is the boundary where a hostile query would either inject
// syntax or crash the query planner. Both are prevented the same way: everything is
// wrapped as a single quoted phrase, with any embedded quote doubled. These cases
// pin that, because the quoting is easy to mistake for redundant and dropping it
// turns every punctuation character in a search box into a query error.
func TestFTSPrefixQuotesEverything(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		input string
		want  string
	}{
		{"bare word gets the prefix wildcard", "nexus", `"nexus"*`},
		{"surrounding space is trimmed", "  nexus  ", `"nexus"*`},
		{"multiple words stay one phrase", "nexus mail", `"nexus mail"*`},
		{"empty input is an empty phrase", "", `""`},
		{"whitespace-only input is an empty phrase", "   ", `""`},
		// The wildcard is withheld once the value carries something FTS5 would read
		// as syntax, because "foo("* is not a valid expression even quoted.
		{"a quote is doubled, not escaped", `say "hi"`, `"say ""hi"""`},
		{"an unbalanced quote is still closed", `unbalanced"`, `"unbalanced"""`},
		{"a bare paren does not become a group", "foo(bar", `"foo(bar"`},
		{"a colon does not become a column filter", "subject:urgent", `"subject:urgent"`},
		{"an asterisk is not a second wildcard", "nexus*", `"nexus*"`},
		{"AND is a literal, not an operator", "foo AND bar", `"foo AND bar"`},
		{"OR is a literal, not an operator", "foo OR bar", `"foo OR bar"`},
		{"NOT is a literal, not an operator", "foo NOT bar", `"foo NOT bar"`},
		// Lowercase operators were never FTS5 syntax, so they keep the wildcard.
		{"lowercase and is an ordinary word", "foo and bar", `"foo and bar"*`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ftsPrefix(testCase.input); got != testCase.want {
				t.Errorf("ftsPrefix(%q) = %q, want %q", testCase.input, got, testCase.want)
			}
		})
	}
}

// escapeLike guards the short-query path, which uses LIKE instead of the FTS index.
// Without the escaping a user searching for "50%" matches every message, and one
// searching for "a_b" matches "axb" — the wildcards belong to LIKE, not to the user.
func TestEscapeLikeNeutralisesWildcards(t *testing.T) {
	for _, testCase := range []struct {
		input string
		want  string
	}{
		{"plain", "plain"},
		{"50%", `50\%`},
		{"a_b", `a\_b`},
		// The backslash has to be doubled first, or escaping the wildcards would
		// itself produce an escape sequence the user did not type.
		{`back\slash`, `back\\slash`},
		{`\%`, `\\\%`},
		{"%_%", `\%\_\%`},
	} {
		if got := escapeLike(testCase.input); got != testCase.want {
			t.Errorf("escapeLike(%q) = %q, want %q", testCase.input, got, testCase.want)
		}
	}
}

// TestSearchSurvivesHostileQueries runs the strings through the real query, because
// ftsPrefix returning a well-formed expression is only half the claim: the other
// half is that SQLite accepts it. An FTS5 syntax error surfaces as a failed query,
// which the transport layer reports as a 500 — a search box that 500s on an
// apostrophe is the failure this protects against.
func TestSearchSurvivesHostileQueries(t *testing.T) {
	store := openTestStore(t)
	account, mailbox := seedAccountMailbox(t, store)
	ctx := context.Background()

	seedMessage(t, store, account.ID, mailbox.ID, 1, "Quarterly report", "incoming", false, 1700000000)
	seedMessage(t, store, account.ID, mailbox.ID, 2, `He said "hello" loudly`, "incoming", false, 1700000100)

	for _, query := range []string{
		`"`, `""`, `"""`, `(`, `)`, `()`, `*`, `**`, `:`, `^`, `-`, `+`,
		"AND", "OR", "NOT", "NEAR", " AND ", "foo AND", "AND bar",
		"NEAR(foo bar)", "subject:urgent", "col:*", `"unclosed`,
		"a'b", "a\\b", "50%", "a_b", "%_%", `\`, `\\`,
		"报告", "报", "  ", "\t\n",
		strings.Repeat("a", 300), strings.Repeat(`"`, 40), strings.Repeat("a ", 200),
	} {
		// Both branches matter: the FTS path and, for queries under three runes, the
		// LIKE path. The rune count decides which one runs, so the short strings here
		// are not redundant with the long ones. UnreadTotal rides along on the page
		// and runs its own predicate, so this covers that query too.
		if _, err := store.ListMessages(ctx, ports.MessageFilter{AccountID: &account.ID, Query: query, Limit: 10}); err != nil {
			t.Errorf("ListMessages with query %q failed: %v", query, err)
		}
	}
}

// TestSearchMatchesWhatItQuotes is the positive half. The tests above establish that
// nothing crashes, which a function returning a constant would also satisfy; these
// establish that the quoting still finds mail. The apostrophe and quote cases are the
// point: they are ordinary in real subjects and each one is a character that would
// break an unparameterised query.
func TestSearchMatchesWhatItQuotes(t *testing.T) {
	store := openTestStore(t)
	account, mailbox := seedAccountMailbox(t, store)
	ctx := context.Background()

	seedMessage(t, store, account.ID, mailbox.ID, 1, "Quarterly report", "incoming", false, 1700000000)
	seedMessage(t, store, account.ID, mailbox.ID, 2, `He said "hello" loudly`, "incoming", false, 1700000100)
	seedMessage(t, store, account.ID, mailbox.ID, 3, "Bob's invoice", "incoming", false, 1700000200)
	seedMessage(t, store, account.ID, mailbox.ID, 4, "季度报告已发布", "incoming", false, 1700000300)

	for _, testCase := range []struct {
		query string
		want  string
	}{
		{"Quarter", "Quarterly report"},   // prefix, via the FTS wildcard
		{"Quarterly", "Quarterly report"}, // whole token
		{`"hello"`, `He said "hello" loudly`},
		{"hello", `He said "hello" loudly`},
		{"Bob's", "Bob's invoice"},
		{"invoice", "Bob's invoice"},
		{"报告", "季度报告已发布"}, // two runes, so this exercises the LIKE branch
	} {
		page, err := store.ListMessages(ctx, ports.MessageFilter{AccountID: &account.ID, Query: testCase.query, Limit: 10})
		if err != nil {
			t.Errorf("ListMessages with query %q failed: %v", testCase.query, err)
			continue
		}
		var found bool
		for _, item := range page.Items {
			if item.Subject == testCase.want {
				found = true
			}
		}
		if !found {
			subjects := make([]string, 0, len(page.Items))
			for _, item := range page.Items {
				subjects = append(subjects, item.Subject)
			}
			t.Errorf("query %q did not find %q, got %v", testCase.query, testCase.want, subjects)
		}
	}

	// A query that matches nothing must come back empty rather than matching
	// everything, which is what a dropped predicate would do.
	page, err := store.ListMessages(ctx, ports.MessageFilter{AccountID: &account.ID, Query: "zzzznotpresent", Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages with an unmatched query failed: %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("an unmatched query returned %d messages, want 0", len(page.Items))
	}
}
