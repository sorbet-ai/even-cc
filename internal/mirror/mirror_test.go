//go:build windows

package mirror

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hinshun/vt10x"
)

type client struct {
	t   *testing.T
	ws  *websocket.Conn
	ctx context.Context
}

func (c *client) send(m map[string]any) {
	b, _ := json.Marshal(m)
	if err := c.ws.Write(c.ctx, websocket.MessageText, b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *client) recv() map[string]any {
	_, b, err := c.ws.Read(c.ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		c.t.Fatalf("decode %s: %v", b, err)
	}
	return m
}

// until reads messages until one of type typ arrives (others go to seen).
func (c *client) until(typ string, seen func(map[string]any)) map[string]any {
	for {
		m := c.recv()
		if m["type"] == typ {
			return m
		}
		if seen != nil {
			seen(m)
		}
	}
}

func TestMirrorDrivesAProgram(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	srv := &Server{Token: "secret", Name: "test-pc", Log: log, Launch: func(preset, cwd string) ([]string, string, []string, error) {
		return []string{os.Getenv("ComSpec"), "/q", "/k", "prompt $G"}, dir, os.Environ(), nil
	}}
	defer srv.Close()
	hs := httptest.NewServer(srv)
	defer hs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// a wrong token is refused
	bad, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hs.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	bc := &client{t, bad, ctx}
	bc.send(map[string]any{"type": "init", "version": 1, "auth_token": "nope", "device": "x", "width": 80, "height": 24})
	if m := bc.recv(); m["type"] != "error" {
		t.Fatalf("bad token got %v", m)
	}
	bad.CloseNow()

	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hs.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	c := &client{t, ws, ctx}
	c.send(map[string]any{"type": "init", "version": 1, "auth_token": "secret", "device": "test glasses", "width": 90, "height": 20})
	if m := c.recv(); m["type"] != "init" || m["server_name"] != "test-pc" {
		t.Fatalf("init reply %v", m)
	}
	c.send(map[string]any{"type": "launch", "command": "shell"})
	launched := c.until("launched", nil)
	sock := launched["socket"].(string)
	c.send(map[string]any{"type": "list"})
	list := c.until("sessions", nil)
	if n := len(list["sessions"].([]any)); n != 1 {
		t.Fatalf("want 1 session, got %v", list)
	}
	c.send(map[string]any{"type": "connect", "socket": sock})
	if m := c.until("connect", nil); m["pid"] == nil {
		t.Fatalf("connect %v", m)
	}
	c.send(map[string]any{"type": "view"})
	snap := c.until("snapshot", nil)
	if snap["width"].(float64) != 90 || snap["height"].(float64) != 20 {
		t.Fatalf("snapshot size %v x %v", snap["width"], snap["height"])
	}

	// the device's emulator: snapshot, then output
	term := vt10x.New(vt10x.WithSize(90, 20))
	apply := func(m map[string]any) {
		b, err := base64.StdEncoding.DecodeString(m["data"].(string))
		if err != nil {
			t.Fatal(err)
		}
		term.Write(b)
	}
	apply(snap)

	// type a command the way Faceclaw submits voice text: text, pause, Enter
	text := "echo mirror-ok-42"
	c.send(map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte(text + "\r")),
		"delays": []map[string]int{{"at": len(text), "ms": 150}}})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		m := c.recv()
		switch m["type"] {
		case "output":
			apply(m)
		case "snapshot":
			term = vt10x.New(vt10x.WithSize(int(m["width"].(float64)), int(m["height"].(float64))))
			apply(m)
		}
		if strings.Count(term.String(), "mirror-ok-42") >= 2 { // the command line and its output
			break
		}
	}
	if strings.Count(term.String(), "mirror-ok-42") < 2 {
		t.Fatalf("command output never appeared; screen:\n%s", term.String())
	}

	// a second view resynchronises with a snapshot showing the same screen
	c.send(map[string]any{"type": "view"})
	snap2 := c.until("snapshot", nil)
	fresh := vt10x.New(vt10x.WithSize(90, 20))
	b, _ := base64.StdEncoding.DecodeString(snap2["data"].(string))
	fresh.Write(b)
	if strings.Count(fresh.String(), "mirror-ok-42") < 2 {
		t.Fatalf("snapshot lost the screen:\n%s", fresh.String())
	}

	// exit ends the session: exit, then disconnected
	c.send(map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte("exit\r"))})
	exit := c.until("exit", nil)
	if exit["status"].(float64) != 0 {
		t.Fatalf("exit %v", exit)
	}
	if m := c.until("disconnected", nil); m["reason"] != "session closed" {
		t.Fatalf("disconnected %v", m)
	}
	c.send(map[string]any{"type": "list"})
	if n := len(c.until("sessions", nil)["sessions"].([]any)); n != 0 {
		t.Fatalf("session still listed after exit")
	}
}

func TestEscScanner(t *testing.T) {
	var e escScanner
	bells, modes := e.scan([]byte("a\x07b\x1b]0;title\x07c\x1b[?2004h\x1b[?1;25l"))
	if bells != 1 {
		t.Fatalf("bells %d", bells)
	}
	want := []modeChange{{2004, true}, {1, false}, {25, false}}
	if len(modes) != len(want) {
		t.Fatalf("modes %v", modes)
	}
	for i := range want {
		if modes[i] != want[i] {
			t.Fatalf("modes %v", modes)
		}
	}
	// split across chunks
	bells, modes = e.scan([]byte("\x1b]2;x"))
	b2, m2 := e.scan([]byte("\x07\x1b[?20"))
	b3, m3 := e.scan([]byte("04l"))
	if bells+b2+b3 != 0 || len(modes)+len(m2) != 0 || len(m3) != 1 || m3[0] != (modeChange{2004, false}) {
		t.Fatalf("split scan: %d %d %d %v %v %v", bells, b2, b3, modes, m2, m3)
	}
}

func TestKeyWords(t *testing.T) {
	for _, c := range []struct {
		text string
		app  bool
		want []string
	}{
		{"down", false, []string{"\x1b[B"}},
		{"Down.", true, []string{"\x1bOB"}},
		{"down, down, enter", false, []string{"\x1b[B", "\x1b[B", "\r"}},
		{"Shift tab", false, []string{"\x1b[Z"}},
		{"two", false, []string{"2"}},
		{"Escape!", false, []string{"\x1b"}},
	} {
		got, ok := keysFor(c.text, c.app)
		if !ok || strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%q: got %q %v", c.text, got, ok)
		}
	}
	for _, text := range []string{"fix the build", "go down to the second file", "", "enter the folder"} {
		if _, ok := keysFor(text, false); ok {
			t.Errorf("%q should be typed, not pressed", text)
		}
	}
}
