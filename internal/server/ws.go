package server

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// ws is GET /api/ws: the same live events as /api/events, one JSON event per
// WebSocket text message, for clients without EventSource (Glassly miniapps
// get fetch and WebSocket only). Auth is the token query parameter, as for SSE.
func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sessionId")
	if sid == "" {
		writeErr(w, 400, "Missing 'sessionId' query parameter")
		return
	}
	// The token is the authentication; native WebSocket bridges send odd or
	// no Origin headers, so origins are not checked (as for SSE).
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		s.Log.Info("ws: upgrade failed", "err", err, "remote", r.RemoteAddr)
		return
	}
	// The connection outlives the server's Write/Read timeouts: net/http's
	// deadlines don't apply to a hijacked connection, so bound writes here.
	ctx := c.CloseRead(context.Background()) // client messages are not used; this handles ping/close
	client := s.Hub.Subscribe(sid, r.URL.Query().Get("needReplay") == "true")
	defer s.Hub.Unsubscribe(sid, client)
	s.Log.Info("ws connected", "session", sid, "clients", s.Hub.ClientCount(), "remote", r.RemoteAddr)
	why := "client closed"
	defer func() {
		s.Log.Info("ws disconnected", "session", sid, "why", why, "clients", s.Hub.ClientCount()-1)
	}()
	send := func(msg []byte) bool {
		wctx, cancel := context.WithTimeout(ctx, SSEWriteTimeout)
		defer cancel()
		if err := c.Write(wctx, websocket.MessageText, msg); err != nil {
			why = "write failed: " + err.Error()
			return false
		}
		return true
	}
	hb := time.NewTicker(HeartbeatInterval)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-client.C:
			if !ok {
				why = "dropped by the server (too slow)"
				c.Close(websocket.StatusTryAgainLater, "too slow")
				return
			}
			client.Took(frame)
			if !send(frameData(frame)) {
				return
			}
		case <-hb.C:
			pctx, cancel := context.WithTimeout(ctx, SSEWriteTimeout)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				why = "ping failed: " + err.Error()
				return
			}
		}
	}
}

// frameData extracts the JSON payload from an SSE frame made by hub.Frame.
func frameData(frame []byte) []byte {
	i := bytes.Index(frame, []byte("\ndata: "))
	if i < 0 {
		return frame
	}
	return bytes.TrimSuffix(frame[i+len("\ndata: "):], []byte("\n\n"))
}
