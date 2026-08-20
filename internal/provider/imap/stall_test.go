//go:build sqlite_fts5

package imap

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"nexusmail/internal/domain"
)

// blackhole is a TCP relay that can stop forwarding bytes while leaving both
// sockets open, which is what a NAT dropping an established flow looks like to
// the process: no RST, no EOF, just silence. Connections opened after the block
// is armed are relayed normally, so recovery by reconnecting is possible.
type blackhole struct {
	target   string
	listener net.Listener
	accepted atomic.Int64
	blockGen atomic.Int64
}

func newBlackhole(t *testing.T, target string) *blackhole {
	t.Helper()
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	hole := &blackhole{target: target, listener: listener}
	go hole.serve()
	t.Cleanup(func() { _ = listener.Close() })
	return hole
}

// block silences every connection accepted so far. Later connections are relayed.
func (b *blackhole) block() { b.blockGen.Store(b.accepted.Load()) }

func (b *blackhole) serve() {
	for {
		downstream, err := b.listener.Accept()
		if err != nil {
			return
		}
		generation := b.accepted.Add(1)
		upstream, err := net.Dial("tcp", b.target)
		if err != nil {
			_ = downstream.Close()
			continue
		}
		go b.relay(downstream, upstream, generation)
		go b.relay(upstream, downstream, generation)
	}
}

func (b *blackhole) relay(from, to net.Conn, generation int64) {
	buffer := make([]byte, 4096)
	for {
		read, err := from.Read(buffer)
		if read > 0 {
			if generation <= b.blockGen.Load() {
				// Swallow the bytes and keep both sockets open: the peer sees its write
				// succeed and then waits for a reply that never comes.
				continue
			}
			if _, writeErr := to.Write(buffer[:read]); writeErr != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				return
			}
			return
		}
	}
}

// TestStalledConnectionRecovers is the regression test for the failure that made
// mail stop arriving entirely. go-imap arms a read deadline only while a response
// is already arriving and cmd.Wait() has no timeout, so a silenced connection used
// to block the command loop forever while it held the command lock: the loop never
// returned to its select, so neither the 5s probe nor client.Closed() could
// recover, and only a restart brought the account back.
func TestStalledConnectionRecovers(t *testing.T) {
	h := newHarness(t)
	hole := newBlackhole(t, serverAddress(t, h))
	h.supervisor.dial = func(ctx context.Context, _ domain.Account) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", hole.listener.Addr().String())
	}
	h.supervisor.commandStall = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.supervisor.Stop() })
	waitConnected(t, h)
	drain(h)

	// Silence the connections the supervisor is already using, then deliver mail it
	// can only see by noticing the stall and reconnecting.
	hole.block()
	arrival := time.Now()
	h.deliver(t, "after-stall")
	_, _ = h.events.await(t, "NEW_EMAIL", 60*time.Second)
	elapsed := time.Since(arrival)
	t.Logf("NEW_EMAIL after %s across a silently stalled connection", elapsed)
	// The stall window plus a reconnect and a sync. Without the guard this never
	// arrives at all.
	if elapsed > 25*time.Second {
		t.Errorf("recovery took %s, want under 25s", elapsed)
	}
}

// serverAddress returns the address of the harness IMAP server by dialling it
// once through the harness hook.
func serverAddress(t *testing.T, h *harness) string {
	t.Helper()
	conn, err := h.supervisor.dial(context.Background(), h.account)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	return conn.RemoteAddr().String()
}
