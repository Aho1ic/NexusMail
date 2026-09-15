package send

import (
	"context"
	"errors"
	"testing"

	"nexusmail/internal/ports"
)

func TestQueueDraftDeliveryRefusesSendingAndMissing(t *testing.T) {
	h := newHarness(t, &backend{})
	ctx := context.Background()

	// A draft already claimed for delivery must not be re-queued under a stale
	// status check — that is the duplicate-send race.
	draft := h.newDraft(t, "race", "body")
	if err := h.repo.QueueDraftDelivery(ctx, draft.ID); err != nil {
		t.Fatalf("initial queue: %v", err)
	}
	if err := h.repo.QueueDraftDelivery(ctx, draft.ID); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("re-queue while queued returned %v, want conflict", err)
	}

	if err := h.repo.QueueDraftDelivery(ctx, draft.ID+9999); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("missing draft returned %v, want not-found", err)
	}
}
