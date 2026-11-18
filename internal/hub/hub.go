// Package hub keeps the per-session event ring buffer and fans events out to
// WebSocket clients (/api/ws).
package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/sorbet-ai/even-cc/internal/jsonx"
)

// MaxMessagesPerSession matches even-terminal's MAX_MESSAGES_PER_SESSION.
const MaxMessagesPerSession = 500

// clientBuffer is how many events a slow client may lag before it is dropped.
const clientBuffer = 4096

// clientMaxBytes caps the bytes queued for one client; a client that lags
// further is dropped (it reconnects and can ask for a replay).
const clientMaxBytes = 8 << 20

// maxRetained caps the ring buffer even during a long turn (normally it keeps
// the last MaxMessagesPerSession events, or everything since the turn began).
const maxRetained = 5000

type entry struct {
	seq  int64 // position in the session's stream, for trimming
	data []byte
}

type sessionBuf struct {
	entries         []entry
	clients         map[*Client]struct{}
	nextSeq         int64
	activeTurnStart int64 // 0 = none
}

// Client is one live connection. The reader calls Took for every event it
// takes from C.
type Client struct {
	C      chan []byte // one JSON event each
	once   sync.Once
	queued atomic.Int64 // bytes in C
}

// Took records that ev left the queue.
func (c *Client) Took(ev []byte) { c.queued.Add(-int64(len(ev))) }

func (c *Client) offer(ev []byte) bool {
	if c.queued.Load()+int64(len(ev)) > clientMaxBytes {
		return false
	}
	select {
	case c.C <- ev:
		c.queued.Add(int64(len(ev)))
		return true
	default:
		return false
	}
}

func (c *Client) close() { c.once.Do(func() { close(c.C) }) }

// Hub is safe for concurrent use. It never calls back into its callers.
type Hub struct {
	mu       sync.Mutex
	sessions map[string]*sessionBuf
	log      *slog.Logger
}

func New(log *slog.Logger) *Hub {
	return &Hub{sessions: map[string]*sessionBuf{}, log: log}
}

func (h *Hub) get(id string) *sessionBuf {
	s := h.sessions[id]
	if s == nil {
		s = &sessionBuf{clients: map[*Client]struct{}{}, nextSeq: 1}
		h.sessions[id] = s
	}
	return s
}

// Emit buffers ev for sessionID and sends it to every connected client.
// Events for an empty session id are dropped, as in even-terminal.
func (h *Hub) Emit(sessionID string, ev any) {
	if sessionID == "" {
		return
	}
	data := jsonx.Marshal(ev)
	var head struct {
		Type  string `json:"type"`
		State string `json:"state"`
	}
	_ = json.Unmarshal(data, &head)

	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.get(sessionID)
	seq := s.nextSeq
	s.nextSeq++
	s.entries = append(s.entries, entry{seq: seq, data: data})
	if head.Type == "user_prompt" && s.activeTurnStart == 0 {
		s.activeTurnStart = seq
	}
	if head.Type == "status" && head.State == "busy" && s.activeTurnStart == 0 {
		s.activeTurnStart = seq
	}
	if head.Type == "status" && head.State == "idle" {
		s.activeTurnStart = 0
	}
	retainFrom := seq - MaxMessagesPerSession + 1
	if s.activeTurnStart != 0 && s.activeTurnStart < retainFrom {
		retainFrom = max(s.activeTurnStart, seq-maxRetained+1)
	}
	drop := 0
	for drop < len(s.entries) && s.entries[drop].seq < retainFrom {
		drop++
	}
	if drop > 0 {
		s.entries = append([]entry(nil), s.entries[drop:]...)
	}

	if h.log != nil && h.log.Enabled(context.Background(), slog.LevelDebug) {
		h.log.Debug("event", "session", sessionID, "data", string(jsonx.TruncateBytes(string(data), 4000)))
	}
	for c := range s.clients {
		if !c.offer(data) {
			delete(s.clients, c)
			c.close()
			if h.log != nil {
				h.log.Warn("client too slow, dropped", "session", sessionID, "queuedBytes", c.queued.Load(), "queuedEvents", len(c.C))
			}
		}
	}
}

// Subscribe attaches a client. With replay, all buffered events are queued
// first (atomically with attaching, so nothing is lost or duplicated).
func (h *Hub) Subscribe(sessionID string, replay bool) *Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.get(sessionID)
	n := clientBuffer
	if replay && len(s.entries)+64 > n {
		n = len(s.entries) + 64
	}
	c := &Client{C: make(chan []byte, n)}
	if replay {
		for _, e := range s.entries {
			c.queued.Add(int64(len(e.data)))
			c.C <- e.data
		}
	}
	s.clients[c] = struct{}{}
	return c
}

// Unsubscribe detaches a client.
func (h *Hub) Unsubscribe(sessionID string, c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.sessions[sessionID]; s != nil {
		if _, ok := s.clients[c]; ok {
			delete(s.clients, c)
			c.close()
		}
	}
}

// ClientCounts returns the connected clients per session (sessions with none
// left out).
func (h *Hub) ClientCounts() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]int{}
	for id, s := range h.sessions {
		if len(s.clients) > 0 {
			out[id] = len(s.clients)
		}
	}
	return out
}

// ClientCount returns the number of clients across all sessions.
func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.sessions {
		n += len(s.clients)
	}
	return n
}
