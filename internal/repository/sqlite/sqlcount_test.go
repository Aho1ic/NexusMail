//go:build sqlite_fts5

package sqlite

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm/logger"
)

// sqlCounter tallies the statements GORM executes, grouped by leading keyword.
//
// The scale tests need to assert that a pass costs one round-trip per chunk
// rather than one per row. Wall-clock time cannot carry that claim: measured on
// this machine, the chunked reconcile pass and a deliberately per-row one land
// at 4.9s and 4.1s under the race detector, so any bound loose enough to be
// stable is also loose enough to pass the regression it is supposed to catch.
// Statement counts are exact, and identical with or without instrumentation.
type sqlCounter struct {
	logger.Interface
	mu     sync.Mutex
	counts map[string]int
}

func newSQLCounter() *sqlCounter {
	return &sqlCounter{Interface: logger.Discard, counts: map[string]int{}}
}

func (c *sqlCounter) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	statement, _ := fc()
	verb := "OTHER"
	if fields := strings.Fields(statement); len(fields) > 0 {
		verb = strings.ToUpper(fields[0])
	}
	c.mu.Lock()
	c.counts[verb]++
	c.mu.Unlock()
}

func (c *sqlCounter) count(verb string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[verb]
}

func (c *sqlCounter) summary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	parts := make([]string, 0, len(c.counts))
	for verb, n := range c.counts {
		parts = append(parts, verb+"="+strconv.Itoa(n))
	}
	slices.Sort(parts)
	return strings.Join(parts, " ")
}

// countSQL routes the store's statements through a counter for the duration of
// fn. GORM copies the config into each session and transaction as it opens
// them, so swapping the logger before fn reaches everything fn runs, and
// restoring it afterwards leaves the rest of the test unaffected.
func countSQL(t *testing.T, store *Store, fn func()) *sqlCounter {
	t.Helper()
	counter := newSQLCounter()
	previous := store.db.Logger
	store.db.Logger = counter
	defer func() { store.db.Logger = previous }()
	fn()
	return counter
}
