package realtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/ports"

	"github.com/coder/websocket"
)

func TestHubPublishesSequenceAndRemovesDisconnectedClient(t *testing.T) {
	hub := New()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		hub.Serve(request.Context(), conn)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForClients(t, hub, 1)
	hub.Publish(ports.Event{Type: "NEW_EMAIL", Data: map[string]any{"message_id": 1}})
	_, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var event ports.Event
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if event.Sequence != 1 || event.OccurredAt == 0 || event.Type != "NEW_EMAIL" {
		t.Fatalf("unexpected event: %#v", event)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	waitForClients(t, hub, 0)
}

// TestHubDeliversAfterIdlePeriod keeps the realtime path honest about long
// quiet periods: a connection that has sat idle past the server's configured
// timeouts must still receive the next published event.
func TestHubDeliversAfterIdlePeriod(t *testing.T) {
	hub := New()
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		hub.Serve(request.Context(), conn)
	})
	server := httptest.NewUnstartedServer(handler)
	server.Config.ReadTimeout = 300 * time.Millisecond
	server.Config.WriteTimeout = 300 * time.Millisecond
	server.Config.IdleTimeout = 300 * time.Millisecond
	server.Start()
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	waitForClients(t, hub, 1)

	// Stay idle well past every server deadline before publishing.
	time.Sleep(time.Second)
	hub.Publish(ports.Event{Type: "NEW_EMAIL", Data: map[string]any{"message_id": 7}})
	_, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read after idle period: %v", err)
	}
	var event ports.Event
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "NEW_EMAIL" {
		t.Fatalf("unexpected event: %#v", event)
	}
}

// TestHubEvictsSlowConsumer covers the eviction branch in Publish. The realtime
// path is publish-then-fan-out, so a single stuck consumer cannot be allowed to
// stall the producer for everyone else. When c.send is full, Publish trips the
// default branch and calls c.stop(); without that branch, a stuck reader would
// back up the channel forever and freeze every subsequent Publish.
func TestHubEvictsSlowConsumer(t *testing.T) {
	hub := New()
	slow := &client{send: make(chan []byte, 64), done: make(chan struct{})}
	hub.mu.Lock()
	hub.clients[slow] = struct{}{}
	hub.mu.Unlock()
	t.Cleanup(func() {
		hub.mu.Lock()
		delete(hub.clients, slow)
		hub.mu.Unlock()
	})

	// Fill the slow client's buffer to capacity. With nobody draining it, the
	// 64th publish leaves the buffer exactly full and the client is still
	// alive — every event so far took the buffered branch.
	for i := 0; i < cap(slow.send); i++ {
		hub.Publish(ports.Event{Type: "NEW_EMAIL", Data: map[string]any{"i": i}})
	}
	if isClientDone(slow) {
		t.Fatal("slow client was evicted before its buffer was full")
	}

	// The next publish cannot enqueue: the buffer is full, so the default
	// branch in Publish fires c.stop() and signals the client to wind down.
	// Map cleanup is owned by the Serve loop's deferred delete; this test
	// exercises the eviction signal Publish is responsible for, so a still-
	// alive Serve loop in production untracks the client on its way out.
	hub.Publish(ports.Event{Type: "NEW_EMAIL", Data: map[string]any{"i": cap(slow.send)}})
	select {
	case <-slow.done:
	case <-time.After(time.Second):
		t.Fatal("slow client was not evicted after its buffer filled")
	}
}

func isClientDone(c *client) bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func waitForClients(t *testing.T, hub *Hub, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hub.mu.RLock()
		current := len(hub.clients)
		hub.mu.RUnlock()
		if current == count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("client count did not become %d", count)
}
