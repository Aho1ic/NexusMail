//go:build sqlite_fts5

package imap

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A fetch that fails has to leave 'error' behind, and the usual reason it failed
// is that its own 45s budget ran out. Writing that state on the expired context
// cannot land, and 'fetching' renders as "will refresh automatically" — the pane
// then promises a retry the prefetch has already given up on.
func TestRollbackCtxSurvivesAnExpiredParent(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	ctx, release := rollbackCtx(parent)
	defer release()

	if err := ctx.Err(); err != nil {
		t.Fatalf("rollback context is already done (%v): the state write cannot land", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("rollback context has no deadline: it is the one write that can outlive shutdown")
	}
	if remaining := time.Until(deadline); remaining > rollbackGrace {
		t.Fatalf("rollback grace is %s, want at most %s", remaining, rollbackGrace)
	}
}

// While the caller's context is still live the rollback rides on it, so the write
// inherits its cancellation instead of being granted a fresh budget.
func TestRollbackCtxReusesALiveParent(t *testing.T) {
	type key struct{}
	parent := context.WithValue(context.Background(), key{}, "carried")

	ctx, release := rollbackCtx(parent)
	defer release()

	if ctx != parent {
		t.Fatal("a live parent was replaced: the rollback no longer inherits its cancellation")
	}

	// Cancelling the parent must reach the returned context.
	cancellable, cancel := context.WithCancel(context.Background())
	derived, releaseDerived := rollbackCtx(cancellable)
	defer releaseDerived()
	cancel()
	if !errors.Is(derived.Err(), context.Canceled) {
		t.Fatalf("derived context saw %v after the parent was cancelled, want context.Canceled", derived.Err())
	}
}
