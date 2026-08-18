package realtime

import (
	"context"
	"encoding/json"
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
		_ = c.conn.CloseNow()
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
			c.stop()
		}
	}
}

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
	for {
		select {
		case <-ctx.Done():
			return
		case <-readCtx.Done():
			return
		case <-c.done:
			return
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
