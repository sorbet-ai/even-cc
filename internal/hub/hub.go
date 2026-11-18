// Package hub keeps the per-session event ring buffer and fans events out to
// SSE clients (docs/even-protocol.md §4.3).
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"

	"even-cc/internal/jsonx"
)

// MaxMessagesPerSession matches even-terminal's MAX_MESSAGES_PER_SESSION.
const MaxMessagesPerSession = 500

// clientBuffer is how many frames a slow client may lag before it is dropped.
const clientBuffer = 4096

// clientMaxBytes caps the bytes queued for one client; a client that lags
// further is dropped (it reconnects and can ask for a replay).
const clientMaxBytes = 8 << 20

// maxRetained caps the ring buffer even during a long turn (normally it keeps
// the last MaxMessagesPerSession events, or everything since the turn began).
const maxRetained = 5000

type entry struct {
	id   int64
	data []byte
}

type sessionBuf struct {
	entries         []entry
	clients         map[*Client]struct{}
	nextID          int64
	activeTurnStart int64 // 0 = none
	reserved        int64 // ids up to this are recorded in the id state file
}

// Client is one SSE connection. The reader calls Took for every frame it
// takes from C.
type Client struct {
	C      chan []byte // complete SSE frames
	once   sync.Once
	queued atomic.Int64 // bytes in C
}

// Took records that frame left the queue.
func (c *Client) Took(frame []byte) { c.queued.Add(-int64(len(frame))) }

func (c *Client) offer(frame []byte) bool {
	if c.queued.Load()+int64(len(frame)) > clientMaxBytes {
		return false
	}
	select {
	case c.C <- frame:
		c.queued.Add(int64(len(frame)))
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
	ids      *idState // nil = ids start at 1 per session (even-terminal)
}

func New(log *slog.Logger) *Hub {
	return &Hub{sessions: map[string]*sessionBuf{}, log: log}
}

func (h *Hub) get(id string) *sessionBuf {
	s := h.sessions[id]
	if s == nil {
		s = &sessionBuf{clients: map[*Client]struct{}{}, nextID: 1}
		if h.ids != nil {
			s.nextID = h.ids.first(id)
			s.reserved = s.nextID - 1
		}
		h.sessions[id] = s
	}
	return s
}

// Frame renders one SSE event frame.
func Frame(id int64, data []byte) []byte {
	b := make([]byte, 0, len(data)+24)
	b = append(b, "id: "...)
	b = strconv.AppendInt(b, id, 10)
	b = append(b, "\ndata: "...)
	b = append(b, data...)
	b = append(b, "\n\n"...)
	return b
}

// Emit buffers ev for sessionID and sends it to every connected client.
// Events for an empty session id are dropped, as in even-terminal.
func (h *Hub) Emit(sessionID string, ev any) int64 {
	if sessionID == "" {
		return 0
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
	id := s.nextID
	s.nextID++
	if h.ids != nil && id > s.reserved {
		s.reserved = id + idBlock - 1
		h.ids.reserve(sessionID, s.reserved, h.log)
	}
	s.entries = append(s.entries, entry{id: id, data: data})
	if head.Type == "user_prompt" && s.activeTurnStart == 0 {
		s.activeTurnStart = id
	}
	if head.Type == "status" && head.State == "busy" && s.activeTurnStart == 0 {
		s.activeTurnStart = id
	}
	if head.Type == "status" && head.State == "idle" {
		s.activeTurnStart = 0
	}
	retainFrom := id - MaxMessagesPerSession + 1
	if s.activeTurnStart != 0 && s.activeTurnStart < retainFrom {
		retainFrom = max(s.activeTurnStart, id-maxRetained+1)
	}
	drop := 0
	for drop < len(s.entries) && s.entries[drop].id < retainFrom {
		drop++
	}
	if drop > 0 {
		s.entries = append([]entry(nil), s.entries[drop:]...)
	}

	if h.log != nil && h.log.Enabled(context.Background(), slog.LevelDebug) {
		h.log.Debug("sse", "session", sessionID, "id", id, "data", string(jsonx.TruncateBytes(string(data), 4000)))
	}
	if len(s.clients) == 0 {
		return id
	}
	frame := Frame(id, data)
	for c := range s.clients {
		if !c.offer(frame) {
			delete(s.clients, c)
			c.close()
			if h.log != nil {
				h.log.Warn("sse client too slow, dropped", "session", sessionID, "queuedBytes", c.queued.Load(), "queuedFrames", len(c.C))
			}
		}
	}
	return id
}

// Subscribe attaches a client. With replay, all buffered frames are queued
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
			f := Frame(e.id, e.data)
			c.queued.Add(int64(len(f)))
			c.C <- f
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

// Messages returns buffered events with id > after as a JSON array of
// {"id":N, ...event} objects (id first, as in /api/messages).
func (h *Hub) Messages(sessionID string, after int64) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	var buf bytes.Buffer
	buf.WriteByte('[')
	first := true
	if s := h.sessions[sessionID]; s != nil {
		for _, e := range s.entries {
			if e.id <= after {
				continue
			}
			if !first {
				buf.WriteByte(',')
			}
			first = false
			buf.WriteString(`{"id":`)
			buf.WriteString(strconv.FormatInt(e.id, 10))
			if len(e.data) > 2 {
				buf.WriteByte(',')
				buf.Write(e.data[1:])
			} else {
				buf.WriteByte('}')
			}
		}
	}
	buf.WriteByte(']')
	return buf.Bytes()
}
