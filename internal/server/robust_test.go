package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"even-cc/internal/events"
	"even-cc/internal/hub"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// A panicking handler answers 500, is logged with its stack, and the server
// keeps serving.
func TestHandlerPanicRecovered(t *testing.T) {
	var logs lockedBuf
	s := &Server{Token: token, Hub: hub.New(nil), Log: slog.New(slog.NewTextHandler(&logs, nil))} // no Mgr: /api/prompt panics
	srv := httptest.NewServer(s)
	defer srv.Close()
	post := func() int {
		req, _ := http.NewRequest("POST", srv.URL+"/api/prompt", strings.NewReader(`{"text":"hi","sessionId":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < 3; i++ {
		if st := post(); st != 500 {
			t.Fatalf("status %d", st)
		}
	}
	resp, err := http.Get(srv.URL + "/api/nonexistent?token=" + token)
	if err != nil || resp.StatusCode != 404 {
		t.Fatalf("server stopped serving: %v", err)
	}
	resp.Body.Close()
	if l := logs.String(); !strings.Contains(l, "panic recovered") || !strings.Contains(l, "http POST /api/prompt") || !strings.Contains(l, "server.go") {
		t.Errorf("log %s", l)
	}
}

// Clients that vanish mid-stream (or never read) are dropped without
// blocking or breaking emits to the others.
func TestWSClientDisconnectMidStream(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	open := func() *websocket.Conn {
		c, _, err := websocket.Dial(ctx, wsURL(e.srv.URL, "sessionId=s1"), nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	gone := open()
	keep := open()
	defer keep.CloseNow()
	deadline := time.Now().Add(2 * time.Second)
	for e.hub.ClientCount() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	gone.CloseNow() // disconnect mid-stream
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				e.hub.Emit("s1", events.TextDelta{Type: "text_delta", Text: strings.Repeat("x", 100)})
			}
		}()
	}
	// a reader that keeps up sees all 2000 events
	got := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for got < 2000 {
			if _, _, err := keep.Read(ctx); err != nil {
				return
			}
			got++
		}
	}()
	wg.Wait()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	if got != 2000 {
		t.Errorf("live client got %d of 2000", got)
	}
	for e.hub.ClientCount() != 1 && time.Now().Before(deadline.Add(3*time.Second)) {
		time.Sleep(time.Millisecond)
	}
	if n := e.hub.ClientCount(); n != 1 {
		t.Errorf("clients %d", n)
	}
}
