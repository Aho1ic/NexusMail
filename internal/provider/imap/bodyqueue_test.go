//go:build sqlite_fts5

package imap

import (
	"context"
	"testing"

	"nexusmail/internal/ports"
)

// fillBodyQueue leaves no room in the prefetch queue, so the enqueue select has to
// take its default branch. Zero is never a real message id, so nothing downstream
// can mistake the padding for work.
func fillBodyQueue(s *Supervisor) {
	for len(s.bodyQueue) < cap(s.bodyQueue) {
		s.bodyQueue <- 0
	}
}

// enqueueBodyCandidates flips its candidates to 'queued' in one batch write before
// offering them to the queue, so a candidate the queue will not accept has already
// been recorded as scheduled work. Leaving it there strands the row: 'queued' is not
// 'ready', so the reading pane keeps waiting for a body no worker will fetch, and the
// bodySeen entry suppresses every later probe for the life of the process.
//
// A full queue is the case that arises under an ordinary prefetch backlog while the
// supervisor is otherwise healthy, which is why it is tested separately from
// shutdown.
func TestAFullQueueReturnsTheCandidateToMetadata(t *testing.T) {
	h := newHarness(t)
	id := seedBodyCandidate(t, h)
	fillBodyQueue(h.supervisor)

	h.supervisor.enqueueBodyCandidates(context.Background(), h.account.ID)

	if state := bodyState(t, h, id); state != "metadata" {
		t.Errorf("body_state = %q after the queue refused the candidate, want %q: the row is stranded in a state no worker clears", state, "metadata")
	}
	if _, seen := h.supervisor.bodySeen.Load(id); seen {
		t.Error("the candidate is still in bodySeen, so every later probe skips it and the body is never fetched again")
	}
}

// cancelAfterMarking is the repository with one seam: the batch write that marks
// candidates 'queued' succeeds and then cancels the caller's context. That is the
// real shutdown interleaving — a context cancelled before the write makes the write
// itself fail, so the rows never reach 'queued' and there is nothing to undo. The
// state that needs undoing only exists in the window between a write that landed and
// a queue send that has not happened yet.
type cancelAfterMarking struct {
	ports.Repository
	cancel context.CancelFunc
}

func (r cancelAfterMarking) BatchSetMessageBodyState(ctx context.Context, ids []int64, state string) error {
	if err := r.Repository.BatchSetMessageBodyState(ctx, ids, state); err != nil {
		return err
	}
	r.cancel()
	return nil
}

// A shutdown arriving in that window is where the rollback grace earns its keep: the
// context the undo would normally ride on is done, so an undo written on it is
// dropped and the row keeps a 'queued' that outlives the process. On the next start
// the row is no longer a candidate the pane can recover, because 'queued' claims work
// that no longer exists.
func TestShutdownDuringEnqueueReturnsTheCandidateToMetadata(t *testing.T) {
	h := newHarness(t)
	id := seedBodyCandidate(t, h)
	fillBodyQueue(h.supervisor)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.supervisor.repo = cancelAfterMarking{Repository: h.repo, cancel: cancel}

	h.supervisor.enqueueBodyCandidates(ctx, h.account.ID)

	if err := ctx.Err(); err == nil {
		t.Fatal("the seam did not cancel the context, so this is not the shutdown window")
	}
	if state := bodyState(t, h, id); state != "metadata" {
		t.Errorf("body_state = %q after a shutdown mid-enqueue, want %q: the undo was written on a context that was already done", state, "metadata")
	}
	if _, seen := h.supervisor.bodySeen.Load(id); seen {
		t.Error("a shutdown left the candidate in bodySeen, so the next process start skips it")
	}
}

// A failed batch write is the one rollback that cannot touch the database, because
// the database is what failed. The rows never reached 'queued', so clearing bodySeen
// is the whole of the repair: the next probe has to be free to reconsider them.
func TestAFailedBatchWriteClearsBodySeen(t *testing.T) {
	h := newHarness(t)
	id := seedBodyCandidate(t, h)

	// Closing the store makes BatchSetMessageBodyState fail for a reason no retry
	// inside the supervisor can fix.
	if err := h.repo.Close(); err != nil {
		t.Fatal(err)
	}

	h.supervisor.enqueueBodyCandidates(context.Background(), h.account.ID)

	if _, seen := h.supervisor.bodySeen.Load(id); seen {
		t.Error("bodySeen still holds a candidate that was never queued, so the next probe skips a body with no pending work")
	}
	if queued := len(h.supervisor.bodyQueue); queued != 0 {
		t.Errorf("%d ids reached the queue after the state write failed, want 0", queued)
	}
}
