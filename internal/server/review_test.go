package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"even-cc/internal/events"
	"even-cc/internal/hub"
)

// Regression tests for the 2026-10-30 engineering review.

func wsURL(base, query string) string {
	return "ws" + strings.TrimPrefix(base, "http") + "/api/ws?token=" + token + "&" + query
}

// A WebSocket outlives the server's read and write timeouts (which bound
// ordinary requests) and its own per-write timeout, as long as the phone
// keeps answering pings.
func TestWSOutlivesServerTimeouts(t *testing.T) {
	WriteTimeout, HeartbeatInterval = 200*time.Millisecond, 50*time.Millisecond
	defer func() { WriteTimeout, HeartbeatInterval = 30*time.Second, 15*time.Second }()
	e := newEnv(t)
	srv := httptest.NewUnstartedServer(e.s)
	srv.Config.ReadTimeout = 150 * time.Millisecond
	srv.Config.WriteTimeout = 150 * time.Millisecond
	srv.Start()
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(srv.URL, "sessionId=s1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	got := make(chan string, 1)
	go func() { // reading also answers the pings
		_, b, err := c.Read(ctx)
		if err != nil {
			got <- err.Error()
			return
		}
		got <- string(b)
	}()
	time.Sleep(600 * time.Millisecond) // past all three timeouts
	e.hub.Emit("s1", events.TextDelta{Type: "text_delta", Text: "late"})
	select {
	case l := <-got:
		if !strings.Contains(l, "late") {
			t.Errorf("got %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("connection died at the server timeouts")
	}
}

// A client that stops reading is dropped once its queue passes the byte cap
// or a write blocks past WriteTimeout; emits never block.
func TestWSStalledClientDropped(t *testing.T) {
	WriteTimeout = 300 * time.Millisecond
	defer func() { WriteTimeout = 30 * time.Second }()
	e := newEnv(t)
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetReadBuffer(4096)
		}
		return c, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(e.srv.URL, "sessionId=s1"), &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{DialContext: dial}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow() // never reads
	deadline := time.Now().Add(2 * time.Second)
	for e.hub.ClientCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	big := strings.Repeat("x", 64<<10)
	start := time.Now()
	for i := 0; i < 400; i++ { // 25 MB, never read
		e.hub.Emit("s1", events.TextDelta{Type: "text_delta", Text: big})
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("emits blocked for %s", d)
	}
	for e.hub.ClientCount() != 0 && time.Now().Before(deadline.Add(5*time.Second)) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := e.hub.ClientCount(); n != 0 {
		t.Errorf("stalled client still attached (%d)", n)
	}
}

func TestHealthEndpoint(t *testing.T) {
	e := newEnv(t)
	st, _, body := e.do("GET", "/api/ecc/health", "", nil)
	var h struct {
		Version  string            `json:"version"`
		Pid      int               `json:"pid"`
		Sessions []json.RawMessage `json:"sessions"`
		Clients  *int              `json:"clients"`
		Panics   *int64            `json:"panics"`
	}
	if st != 200 || json.Unmarshal([]byte(body), &h) != nil || h.Version != "0.1.0" || h.Pid == 0 || h.Sessions == nil || h.Clients == nil || h.Panics == nil {
		t.Errorf("health %d %s", st, body)
	}
	if st, _, _ := e.do("GET", "/api/ecc/health", "", map[string]string{}); st != 401 {
		t.Errorf("health without token: %d", st)
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	e := newEnv(t)
	body := `{"text":"` + strings.Repeat("x", maxBody) + `"}`
	if st, _, resp := e.do("POST", "/api/prompt", body, nil); st != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d %.200s", st, resp)
	}
}

func TestAuthFailureLogRateLimited(t *testing.T) {
	var logs lockedBuf
	s := &Server{Token: token, Hub: hub.New(nil), Log: slog.New(slog.NewTextHandler(&logs, nil))}
	srv := httptest.NewServer(s)
	defer srv.Close()
	for i := 0; i < 30; i++ {
		resp, err := http.Get(srv.URL + "/api/sessions?token=wrong")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	if n := strings.Count(logs.String(), `msg="auth failed"`); n != authLogBurst {
		t.Errorf("logged %d auth failures, want %d", n, authLogBurst)
	}
}

func TestRingBufferCappedDuringLongTurn(t *testing.T) {
	h := hub.New(nil)
	h.Emit("s", events.NewUserPrompt("p"))
	for i := 0; i < 6000; i++ {
		h.Emit("s", events.TextDelta{Type: "text_delta", Text: "x"})
	}
	if all := buffered(h, "s"); len(all) != 5000 {
		t.Errorf("retained %d during the turn", len(all))
	}
}
