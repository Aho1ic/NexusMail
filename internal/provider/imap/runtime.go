package imap

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"nexusmail/internal/domain"

	"github.com/emersion/go-imap/v2/imapclient"
)

// Per-account runtime state and the two-level lock over the command connection.

type runtime struct {
	account domain.Account
	cancel  context.CancelFunc
	syncReq chan int64
	cmdMu   sync.Mutex
	// urgent counts foreground waiters for cmdMu. Background body prefetch
	// yields while it is non-zero so a new-mail sync never queues behind a
	// backlog of body fetches.
	urgent atomic.Int32
	client atomic.Pointer[imapclient.Client]
}

// lock claims the command connection for sync or user-facing work.
func (rt *runtime) lock() {
	rt.urgent.Add(1)
	rt.cmdMu.Lock()
	rt.urgent.Add(-1)
}

func (rt *runtime) unlock() { rt.cmdMu.Unlock() }

// lockBackground claims the command connection for opportunistic prefetch and
// steps aside whenever foreground work is waiting.
func (rt *runtime) lockBackground(ctx context.Context) bool {
	for ctx.Err() == nil {
		if rt.urgent.Load() == 0 {
			rt.cmdMu.Lock()
			if rt.urgent.Load() == 0 {
				return true
			}
			rt.cmdMu.Unlock()
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(25 * time.Millisecond):
		}
	}
	return false
}

func (s *Supervisor) requestSync(rt *runtime) {
	select {
	case rt.syncReq <- 0:
	default:
	}
}

func (s *Supervisor) RequestMailbox(ctx context.Context, mailboxID int64) error {
	mailbox, err := s.repo.GetMailbox(ctx, mailboxID)
	if err != nil {
		return err
	}
	rt, err := s.runtime(mailbox.AccountID)
	if err != nil {
		return err
	}
	now := time.Now()
	if prev, ok := s.lastSyncReq.Load(mailboxID); ok {
		if now.Sub(prev.(time.Time)) < requestSyncCooldown {
			return nil
		}
	}
	s.lastSyncReq.Store(mailboxID, now)
	select {
	case rt.syncReq <- mailboxID:
	default:
	}
	return nil
}

func (s *Supervisor) closeCommand(rt *runtime, client *imapclient.Client) {
	rt.client.CompareAndSwap(client, nil)
	_ = client.Close()
}
