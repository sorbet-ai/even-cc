// mirror-e2e drives even-cc's g2mirror endpoint the way Faceclaw's Terminal
// app does: init at the glasses' grid, launch a preset, connect, view, type
// a prompt (text, pause, Enter) and wait for a reply on the emulated screen.
// Use it against a test even-cc only (never the live one):
//
//	go run ./tools/mirror-e2e ws://127.0.0.1:8738 <token> "<prompt>" "<expected text>"
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hinshun/vt10x"
)

const cols, rows = 100, 35 // Faceclaw's Terminus 6x12 on the full panel

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: mirror-e2e ws://host:port token prompt expected")
		os.Exit(2)
	}
	url, token, prompt, want := os.Args[1], os.Args[2], os.Args[3], os.Args[4]
	if strings.HasSuffix(url, ":8737") {
		fail("refusing the default port: point this at a test even-cc")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		fail("dial: %v", err)
	}
	ws.SetReadLimit(8 << 20)
	t0 := time.Now()
	logf := func(f string, a ...any) { fmt.Printf("%5.1fs %s\n", time.Since(t0).Seconds(), fmt.Sprintf(f, a...)) }
	send := func(m map[string]any) {
		b, _ := json.Marshal(m)
		if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
			fail("write: %v", err)
		}
	}
	// one reader: a timed-out websocket read would close the connection
	msgs := make(chan map[string]any, 1024)
	go func() {
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				close(msgs)
				return
			}
			var m map[string]any
			json.Unmarshal(b, &m)
			msgs <- m
		}
	}()
	recv := func() map[string]any {
		m, ok := <-msgs
		if !ok {
			fail("connection closed")
		}
		return m
	}
	until := func(typ string) map[string]any {
		for {
			if m := recv(); m["type"] == typ {
				return m
			} else if m["type"] == "error" {
				fail("error: %v", m["message"])
			}
		}
	}

	send(map[string]any{"type": "init", "version": 1, "auth_token": token, "device": "mirror-e2e", "width": cols, "height": rows})
	logf("init: %v", until("init"))
	send(map[string]any{"type": "launch", "command": "shell"})
	sock := until("launched")["socket"].(string)
	logf("launched %s", sock)
	send(map[string]any{"type": "connect", "socket": sock})
	logf("connect: command=%v cwd=%v", until("connect")["command"], "")
	send(map[string]any{"type": "view"})
	snap := until("snapshot")
	term := vt10x.New(vt10x.WithSize(cols, rows))
	apply := func(m map[string]any) {
		b, _ := base64.StdEncoding.DecodeString(m["data"].(string))
		term.Write(b)
	}
	apply(snap)
	frames := 0
	// read output until the screen shows text, a predicate holds, or time runs out
	pump := func(d time.Duration, done func(string) bool) bool {
		end := time.Now().Add(d)
		timer := time.NewTimer(d)
		defer timer.Stop()
		for time.Now().Before(end) {
			var m map[string]any
			select {
			case <-timer.C:
				return done(term.String())
			case mm, ok := <-msgs:
				if !ok {
					fail("connection closed")
				}
				m = mm
			}
			switch m["type"] {
			case "output":
				apply(m)
				frames++
			case "snapshot":
				term = vt10x.New(vt10x.WithSize(int(m["width"].(float64)), int(m["height"].(float64))))
				apply(m)
			case "exit", "disconnected":
				fail("session ended: %v", m)
			}
			if done(term.String()) {
				return true
			}
		}
		return done(term.String())
	}
	// Claude Code is up when its input prompt shows; a first run in a new
	// folder asks to trust it first
	prompt0 := func(s string) bool {
		return strings.Contains(s, "shift+tab") || strings.Contains(s, "? for shortcuts")
	}
	ready := func(s string) bool { return prompt0(s) || strings.Contains(s, "trust") }
	if !pump(60*time.Second, ready) {
		fail("claude never became ready; screen:\n%s", term.String())
	}
	if strings.Contains(term.String(), "trust") && !prompt0(term.String()) {
		logf("trust prompt shown; saying \"down\" then \"enter\" like Faceclaw voice input")
		pump(time.Second, func(string) bool { return false }) // let it settle
		for _, w := range []string{"Down.", "Enter"} {
			send(map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte(w + "")),
				"delays": []map[string]int{{"at": len(w), "ms": 150}}})
			pump(700*time.Millisecond, func(string) bool { return false })
		}
		if !pump(30*time.Second, prompt0) {
			fail("no prompt after trust; screen:\n%s", term.String())
		}
	}
	logf("claude ready (%d output messages)", frames)
	send(map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte(prompt + "\r")),
		"delays": []map[string]int{{"at": len(prompt), "ms": 150}}})
	logf("sent prompt")
	// done: the reply is on screen and claude is no longer working
	if !pump(150*time.Second, func(s string) bool {
		return strings.Count(s, want) >= 2 && !strings.Contains(s, "esc to interrupt") && prompt0(s)
	}) {
		fail("reply never appeared; screen:\n%s", term.String())
	}
	logf("reply on screen (%d output messages)", frames)
	// a re-view gives a snapshot equal to what the stream built
	send(map[string]any{"type": "view"})
	snap = until("snapshot")
	fresh := vt10x.New(vt10x.WithSize(int(snap["width"].(float64)), int(snap["height"].(float64))))
	b, _ := base64.StdEncoding.DecodeString(snap["data"].(string))
	fresh.Write(b)
	if !strings.Contains(fresh.String(), want) {
		fail("snapshot lost the reply:\n%s", fresh.String())
	}
	os.WriteFile("mirror-e2e-screen.txt", []byte(term.String()), 0o644)
	fmt.Println(term.String())
	send(map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte("\x03\x03"))}) // Ctrl+C twice quits claude
	logf("quit: %v", until("exit"))
	logf("done")
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "FAIL: "+f+"\n", a...)
	os.Exit(1)
}
