package realtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nexusmail/internal/ports"

	"github.com/coder/websocket"
)

// hubServer runs the hub behind a real websocket endpoint and returns its URL.
func hubServer(t *testing.T, hub *Hub) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		hub.Serve(request.Context(), conn)
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// Publish walks the client map under a read lock while Serve mutates it under a
// write lock. This drives both at once, which is the shape that would surface a
// concurrent map access or a send on a closed channel.
func TestHubSurvivesConcurrentPublishAndConnect(t *testing.T) {
	hub := New()
	url := hubServer(t, hub)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	stop := make(chan struct{})
	var published atomic.Int64
	var publishers sync.WaitGroup
	for i := 0; i < 4; i++ {
		publishers.Add(1)
		go func() {
			defer publishers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				hub.Publish(ports.Event{Type: "NEW_EMAIL", Data: map[string]any{"n": published.Add(1)}})
			}
		}()
	}

	// Connect and disconnect while the publishers run.
	var clients sync.WaitGroup
	for i := 0; i < 16; i++ {
		clients.Add(1)
		go func() {
			defer clients.Done()
			for round := 0; round < 4; round++ {
				conn, _, err := websocket.Dial(ctx, url, nil)
				if err != nil {
					return
				}
				// Read a little so the connection is doing real work, then drop it
				// mid-stream, which is what a closed browser tab looks like.
				readCtx, readCancel := context.WithTimeout(ctx, 200*time.Millisecond)
				_, _, _ = conn.Read(readCtx)
				readCancel()
				_ = conn.Close(websocket.StatusNormalClosure, "done")
			}
		}()
	}
	clients.Wait()
	close(stop)
	publishers.Wait()

	if published.Load() == 0 {
		t.Fatal("no events were published")
	}
	// Every client is gone, so the map must be empty: a leaked entry means Publish
	// keeps writing into a dead socket forever.
	waitForClients(t, hub, 0)
}

// Sequence numbers come from an atomic counter, so under concurrent publishers
// every event still gets a distinct one and none is skipped. A client uses the
// sequence to detect a gap, so a duplicate would make it believe it is up to date
// when it is not.
func TestHubSequencesAreUniqueUnderConcurrentPublishers(t *testing.T) {
	hub := New()
	url := hubServer(t, hub)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")
	waitForClients(t, hub, 1)

	// The client buffer holds 64, and a slow consumer is evicted, so the total
	// stays under that: this test is about sequence allocation, not backpressure.
	const publishers, each = 8, 6
	var wg sync.WaitGroup
	for i := 0; i < publishers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				hub.Publish(ports.Event{Type: "MESSAGE_UPDATED"})
			}
		}()
	}
	wg.Wait()

	seen := map[uint64]bool{}
	for i := 0; i < publishers*each; i++ {
		readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
		_, payload, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		var event ports.Event
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatal(err)
		}
		if event.Sequence == 0 {
			t.Fatal("an event carried sequence 0")
		}
		if seen[event.Sequence] {
			t.Fatalf("sequence %d was delivered twice", event.Sequence)
		}
		seen[event.Sequence] = true
	}
	if len(seen) != publishers*each {
		t.Fatalf("saw %d distinct sequences, want %d", len(seen), publishers*each)
	}
}

// One client whose buffer has filled must not hold up the others: Publish walks
// every client under one read lock, so a blocking send would stall the whole fan
// out. The stalled client is synthetic because a real socket cannot be made slow
// with small frames — the kernel buffer absorbs them — and what is being checked
// here is that eviction of one does not cost the others their delivery.
func TestHubSlowClientDoesNotBlockTheOthers(t *testing.T) {
	hub := New()
	url := hubServer(t, hub)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	stalled := &client{send: make(chan []byte, 64), done: make(chan struct{})}
	hub.mu.Lock()
	hub.clients[stalled] = struct{}{}
	hub.mu.Unlock()
	t.Cleanup(func() {
		hub.mu.Lock()
		delete(hub.clients, stalled)
		hub.mu.Unlock()
	})

	fast, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close(websocket.StatusNormalClosure, "done")
	waitForClients(t, hub, 2)

	// The healthy client drains continuously.
	sawFinal := make(chan struct{})
	readFailed := make(chan error, 1)
	go func() {
		for {
			readCtx, readCancel := context.WithTimeout(ctx, 10*time.Second)
			_, payload, err := fast.Read(readCtx)
			readCancel()
			if err != nil {
				select {
				case readFailed <- err:
				default:
				}
				return
			}
			var event ports.Event
			if err := json.Unmarshal(payload, &event); err != nil {
				return
			}
			if event.Type == "NEW_EMAIL" {
				close(sawFinal)
				return
			}
		}
	}()

	// Fill the stalled client's 64-slot buffer and publish one more, which is the
	// event that cannot be enqueued. Publish must not block on it, so the loop has
	// to finish promptly. Kept to exactly the buffer size plus one so the healthy
	// client's own buffer is never at risk.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i <= cap(stalled.send); i++ {
			hub.Publish(ports.Event{Type: "MESSAGE_UPDATED", Data: map[string]any{"n": i}})
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Publish blocked on a client whose buffer was full")
	}
	if !isClientDone(stalled) {
		t.Fatal("the stalled client was never evicted")
	}

	// The healthy client is still connected and still receives new events.
	hub.Publish(ports.Event{Type: "NEW_EMAIL"})
	select {
	case <-sawFinal:
	case err := <-readFailed:
		t.Fatalf("the healthy client stopped receiving: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the healthy client never received the event published after the eviction")
	}
}

// Publishing with no clients connected must be free and must not block: the send
// worker and the supervisor publish on every state change, including at startup
// before any browser has connected.
func TestHubPublishWithNoClients(t *testing.T) {
	hub := New()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10000; i++ {
			hub.Publish(ports.Event{Type: "MESSAGE_UPDATED"})
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Publish blocked with no clients connected")
	}
}

// A cancelled context must take the client down and remove it from the map, so a
// shutdown does not leave the hub holding sockets it will never write to again.
func TestHubServeExitsOnContextCancel(t *testing.T) {
	hub := New()
	serveCtx, cancelServe := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		hub.Serve(serveCtx, conn)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")
	waitForClients(t, hub, 1)

	cancelServe()
	waitForClients(t, hub, 0)
}
