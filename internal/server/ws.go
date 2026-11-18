package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// HeartbeatInterval between WebSocket pings.
var HeartbeatInterval = 15 * time.Second

// WriteTimeout bounds each WebSocket write and ping: a phone that vanished
// from the network is dropped after this, not after the TCP retransmission
// timeout.
var WriteTimeout = 30 * time.Second

// ws is GET /api/ws: a session's live events, one JSON event per WebSocket
// text message; needReplay=true sends the buffered ones first. Auth is the
// token query parameter.
func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sessionId")
	if sid == "" {
		writeErr(w, 400, "Missing 'sessionId' query parameter")
		return
	}
	// The token is the authentication; native WebSocket bridges send odd or
	// no Origin headers, so origins are not checked.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		s.Log.Info("ws: upgrade failed", "err", err, "remote", r.RemoteAddr)
		return
	}
	// The connection outlives the server's Write/Read timeouts: net/http's
	// deadlines don't apply to a hijacked connection, so bound writes here.
	ctx := c.CloseRead(context.Background()) // client messages are not used; this handles ping/close
	replay := r.URL.Query().Get("needReplay") == "true"
	client := s.Hub.Subscribe(sid, replay)
	defer s.Hub.Unsubscribe(sid, client)
	var pending []any // prompts waiting for an answer: a client (re)connecting mid-wait must see them
	if sess := s.Mgr.Lookup(sid); sess != nil && !replay {
		pending = sess.PendingEvents()
	}
	s.Log.Info("ws connected", "session", sid, "clients", s.Hub.ClientCount(), "remote", r.RemoteAddr)
	why := "client closed"
	defer func() {
		s.Log.Info("ws disconnected", "session", sid, "why", why, "clients", s.Hub.ClientCount()-1)
	}()
	send := func(msg []byte) bool {
		wctx, cancel := context.WithTimeout(ctx, WriteTimeout)
		defer cancel()
		if err := c.Write(wctx, websocket.MessageText, msg); err != nil {
			why = "write failed: " + err.Error()
			return false
		}
		return true
	}
	for _, ev := range pending {
		if b, err := json.Marshal(ev); err == nil && !send(b) {
			return
		}
	}
	hb := time.NewTicker(HeartbeatInterval)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-client.C:
			if !ok {
				why = "dropped by the server (too slow)"
				c.Close(websocket.StatusTryAgainLater, "too slow")
				return
			}
			client.Took(ev)
			if !send(ev) {
				return
			}
		case <-hb.C:
			pctx, cancel := context.WithTimeout(ctx, WriteTimeout)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				why = "ping failed: " + err.Error()
				return
			}
		}
	}
}
