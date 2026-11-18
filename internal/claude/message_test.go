package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMessageLines(t *testing.T) {
	b, err := messageLines("0123456789abcdef0123456789abcdef", heldID, "check the logs")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 2 || lines[0] != `{"token":"0123456789abcdef0123456789abcdef","type":"auth"}` {
		t.Fatalf("%q", lines)
	}
	var msg struct {
		Type, SessionID, Priority string
		Message                   struct{ Content string }
	}
	if err := json.Unmarshal([]byte(strings.ReplaceAll(lines[1], `"session_id"`, `"sessionid"`)), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "user" || msg.SessionID != heldID || msg.Message.Content != "check the logs" || msg.Priority != "next" {
		t.Fatalf("%+v", msg)
	}
	if _, err := messageLines("t", heldID, ""); err == nil {
		t.Error("empty text must fail")
	}
}

// A held session gets the message on its inbox (a Unix socket stands in).
func TestMessageElsewhereWritesToTheInbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes: checked live with tools/message-probe")
	}
	h := newHarness(t, Display{})
	dir := filepath.Dir(h.store.ProjectsDir())
	h.m.opts.ClaudeDir = dir
	pid := holder(t)
	sock := filepath.Join(t.TempDir(), "inbox.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan []string, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		sc := bufio.NewScanner(c)
		var ls []string
		for sc.Scan() {
			ls = append(ls, sc.Text())
		}
		got <- ls
	}()
	sessions := filepath.Join(dir, "sessions")
	_ = os.MkdirAll(sessions, 0o755)
	body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"procStart":%q,"entrypoint":"cli","pidDomain":"%s:h","messagingSocketPath":%q}`,
		pid, heldID, processStart(pid), pidPlatform, sock)
	_ = os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", pid)), []byte(body), 0o644)
	_ = os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.%s.key", pid, strings.Repeat("a", 64))),
		[]byte(`{"peerToken":"0123456789abcdef0123456789abcdef"}`), 0o600)
	if err := h.m.MessageElsewhere(heldID, "hello there"); err != nil {
		t.Fatal(err)
	}
	ls := <-got
	if len(ls) != 2 || !strings.Contains(ls[0], `"auth"`) || !strings.Contains(ls[1], `"hello there"`) {
		t.Fatalf("%q", ls)
	}
	if err := h.m.MessageElsewhere("33333333-3333-4333-8333-333333333333", "x"); err == nil {
		t.Error("a session nobody holds can't be messaged")
	}
}
