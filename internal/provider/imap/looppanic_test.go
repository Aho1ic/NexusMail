//go:build sqlite_fts5

package imap

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"nexusmail/internal/ports"
)

// commandLoop and idleLoop walk provider responses and write to the database on
// every iteration, and neither had a recover. A panic there terminated the whole
// process, taking the other accounts, the send worker and the four body workers
// with it. The prefetch workers already carry this guard for the same reason.
//
// The policy under test is park, not retry: the account goes to `error` — rendered
// as "同步出错，请重启网关后重试" — and the loop stays down. `backoff` is the status that
// promises a retry, and a panic is a bug in this code rather than a transient
// provider fault, so it must not claim one is coming.

// TestPanickedLoopParksTheAccountAndStaysDown drives runAccountLoop directly. A
// loop that panics once must be recovered, must not be started again, and must
// leave the account in `error` with no last_error: the panic value can carry a
// message body or a credential and must never reach the client.
func TestPanickedLoopParksTheAccountAndStaysDown(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var starts atomic.Int32
	loop := func(context.Context, *runtime) {
		starts.Add(1)
		panic("synthetic panic carrying secret-body-text")
	}

	// Registered the way StartAccount would, so the recover's StopAccount has a
	// runtime to retire and a cancel to call.
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	rt := &runtime{account: h.account, cancel: loopCancel, syncReq: make(chan int64, 8)}
	h.supervisor.mu.Lock()
	h.supervisor.runtimes[h.account.ID] = rt
	h.supervisor.mu.Unlock()

	done := make(chan struct{})
	go func() { defer close(done); h.supervisor.runAccountLoop(loopCtx, rt, "test", loop) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runAccountLoop never returned after the panic")
	}

	waitFor(t, 10*time.Second, func() bool {
		account, err := h.repo.GetAccount(context.Background(), h.account.ID)
		return err == nil && account.Status == "error"
	})
	account, err := h.repo.GetAccount(context.Background(), h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "error" {
		t.Errorf("account status is %q, want error", account.Status)
	}
	if account.LastError != nil {
		t.Errorf("last_error is %q, want empty: the panic value may carry mail or credentials", *account.LastError)
	}

	// Parked, not retried. `backoff` is the status that means a retry is coming.
	time.Sleep(500 * time.Millisecond)
	if got := starts.Load(); got != 1 {
		t.Errorf("the loop ran %d times, want exactly 1: a recovered panic must not restart", got)
	}

	// The runtime is retired, so a later StartAccount can bring the account back
	// rather than being refused as already running.
	if _, err := h.supervisor.runtime(h.account.ID); err == nil {
		t.Error("the panicked account still has a registered runtime, so it can never be restarted")
	}
}

// panicOncePublisher panics the first time it sees a given event type, then
// behaves like the wrapped publisher. It models a bug in the loop's own
// goroutine: flushPending publishes NEW_EMAIL while holding the command lock, so
// a panic there is both a crash of commandLoop and a held mutex.
type panicOncePublisher struct {
	inner ports.Publisher
	kind  string
	fired atomic.Bool
}

func (p *panicOncePublisher) Publish(event ports.Event) {
	if event.Type == p.kind && p.fired.CompareAndSwap(false, true) {
		panic("synthetic panic on " + p.kind)
	}
	p.inner.Publish(event)
}

// TestCommandLoopPanicDuringIngestParksTheAccount is the end-to-end half: the
// panic happens inside real ingest work, under the command lock, on a supervisor
// started the way main.go starts it. The process must survive, the account must
// park in `error`, and the command lock must be free afterwards — recovering
// without releasing it would leave every user action on this account blocked,
// which is a quieter outage than the crash the recover was added to prevent.
func TestCommandLoopPanicDuringIngestParksTheAccount(t *testing.T) {
	h := newHarness(t)
	wrapper := &panicOncePublisher{inner: h.supervisor.events, kind: "NEW_EMAIL"}
	h.supervisor.events = wrapper

	h.deliver(t, "panic-trigger")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()

	// The runtime is captured before the panic retires it, because the lock
	// assertion below needs the same runtime the panicking loop was holding.
	var rt *runtime
	waitFor(t, 30*time.Second, func() bool {
		value, err := h.supervisor.runtime(h.account.ID)
		if err != nil {
			return false
		}
		rt = value
		return true
	})

	waitFor(t, 30*time.Second, func() bool { return wrapper.fired.Load() })
	waitFor(t, 30*time.Second, func() bool {
		account, err := h.repo.GetAccount(context.Background(), h.account.ID)
		return err == nil && account.Status == "error"
	})

	locked := make(chan struct{})
	go func() {
		rt.lock()
		close(locked)
		rt.unlock()
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("the command lock was still held after the panic, so every action on this account would block")
	}
}
