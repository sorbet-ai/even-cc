package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"even-cc/internal/events"
)

func TestWebSocketEvents(t *testing.T) {
	e := newEnv(t)
	HeartbeatInterval = 50 * time.Millisecond
	defer func() { HeartbeatInterval = 15 * time.Second }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	base := "ws" + strings.TrimPrefix(e.srv.URL, "http")

	if _, resp, err := websocket.Dial(ctx, base+"/api/ws?sessionId=s1&token=wrong", nil); err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("bad token: err %v resp %v", err, resp)
	}

	e.hub.Emit("s1", events.TextDelta{Type: "text_delta", Text: "a<b>& "})
	c, _, err := websocket.Dial(ctx, base+"/api/ws?sessionId=s1&needReplay=true&token="+token, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	read := func() string {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ != websocket.MessageText {
			t.Errorf("message type %v", typ)
		}
		return string(b)
	}
	if got := read(); got != `{"type":"text_delta","text":"a<b>& "}` {
		t.Errorf("replay %q", got)
	}
	// heartbeats are pings, answered by the client's read loop, not messages
	time.Sleep(150 * time.Millisecond)
	e.hub.Emit("s1", events.NewStatus("idle", "s1"))
	if got := read(); got != `{"type":"status","state":"idle","sessionId":"s1","provider":"claude"}` {
		t.Errorf("live %q", got)
	}
	if n := e.hub.ClientCount(); n != 1 {
		t.Errorf("clients %d", n)
	}
	c.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(2 * time.Second)
	for e.hub.ClientCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := e.hub.ClientCount(); n != 0 {
		t.Errorf("client not unsubscribed after close: %d", n)
	}

	// without needReplay only new events arrive
	c2, _, err := websocket.Dial(ctx, base+"/api/ws?sessionId=s1&token="+token, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.CloseNow()
	for e.hub.ClientCount() != 1 && time.Now().Before(deadline.Add(2*time.Second)) {
		time.Sleep(time.Millisecond)
	}
	e.hub.Emit("s1", events.TextDelta{Type: "text_delta", Text: "new"})
	if _, b, err := c2.Read(ctx); err != nil || string(b) != `{"type":"text_delta","text":"new"}` {
		t.Errorf("no-replay %q %v", b, err)
	}
}
