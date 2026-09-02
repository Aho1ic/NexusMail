package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"nexusmail/internal/ports"

	"github.com/coder/websocket"
)

type Hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
	seq     atomic.Uint64
}

type client struct {
	conn *websocket.Conn
	send chan []byte
	done chan struct{}
	once sync.Once
}

func (c *client) stop() {
	c.once.Do(func() {
		close(c.done)
		if c.conn != nil {
			_ = c.conn.CloseNow()
		}
	})
}

func New() *Hub { return &Hub{clients: make(map[*client]struct{})} }

func (h *Hub) Publish(event ports.Event) {
	event.Sequence = h.seq.Add(1)
	if event.OccurredAt == 0 {
		event.OccurredAt = time.Now().UnixMilli()
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- payload:
		default:
			// A client that cannot take an event within a 64-deep buffer is not
			// reading, so it is dropped rather than allowed to block every other
			// subscriber behind it. Logged because the browser sees this as a
			// socket that closed for no stated reason and reconnects, and without
			// a line here there is nothing on the server that says why.
			slog.Warn("realtime client dropped: send buffer full",
				"buffer", cap(c.send), "event_type", event.Type, "sequence", event.Sequence)
			c.stop()
		}
	}
}

// keepaliveInterval bounds how long a silently dead connection can look
// healthy. Without it a client can wait indefinitely for events that are being
// written into a broken socket.
const keepaliveInterval = 20 * time.Second

func (h *Hub) Serve(ctx context.Context, conn *websocket.Conn) {
	readCtx := conn.CloseRead(ctx)
	c := &client{conn: conn, send: make(chan []byte, 64), done: make(chan struct{})}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
		c.stop()
	}()
	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-readCtx.Done():
			return
		case <-c.done:
			return
		case <-keepalive.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}
		case payload := <-c.send:
			writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := conn.Write(writeCtx, websocket.MessageText, payload)
			cancel()
			if err != nil {
				return
			}
		}
	}
}
