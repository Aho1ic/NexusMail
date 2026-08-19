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
