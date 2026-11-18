package claude

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

// registry writes a Claude Code live-session entry.
func registry(t *testing.T, dir string, pid int, sid, procStart, entrypoint string) {
	t.Helper()
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"procStart":%q,"kind":"interactive","entrypoint":%q,"pidDomain":"%s:host"}`,
		pid, sid, procStart, entrypoint, pidPlatform)
	if err := os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d-%s.json", pid, sid[:4])), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	heldID  = "11111111-1111-4111-8111-111111111111"
	staleID = "22222222-2222-4222-8222-222222222222"
	ownID   = "33333333-3333-4333-8333-333333333333"
	deadID  = "44444444-4444-4444-8444-444444444444"
)

// The entries' pidDomain is "<platform>:host", with no pid namespace.
// heldHarness: heldID is open in "VS Code" (our parent process stands in for
// it: alive, not ours); the other entries must not count.
func heldHarness(t *testing.T) *harness {
	h := newHarness(t, Display{})
	dir := filepath.Dir(h.store.ProjectsDir())
	h.m.opts.ClaudeDir = dir
	other := os.Getppid()
	registry(t, dir, other, heldID, processStart(other), "claude-vscode")
	registry(t, dir, other, staleID, "1", "cli")       // pid reused since
	registry(t, dir, os.Getpid(), ownID, "", "sdk-ts") // this server's own
	registry(t, dir, 1<<30, deadID, "", "cli")         // gone
	writeTranscript(t, h.store, heldID, h.cwd, `{"type":"user","uuid":"u1","cwd":`+fmt.Sprintf("%q", h.cwd)+`,"message":{"role":"user","content":"hi"}}`)
	return h
}

func TestOpenElsewhere(t *testing.T) {
	h := heldHarness(t)
	got := h.m.OpenElsewhere()
	if e, ok := got[heldID]; !ok || e.Where != "VS Code" {
		t.Fatalf("held session: %+v", got)
	}
	for _, id := range []string{ownID, deadID, staleID} {
		if _, ok := got[id]; ok {
			t.Errorf("%s counted as open elsewhere", id)
		}
	}
	h.m.opts.ClaudeDir = ""
	if len(h.m.OpenElsewhere()) != 0 {
		t.Error("no claude dir: nothing to check")
	}
}

func TestPromptToHeldSessionIsRefused(t *testing.T) {
	h := heldHarness(t)
	_, err := h.m.Prompt(heldID, "hello", nil)
	var he *HTTPError
	if !errors.As(err, &he) || he.Code != 409 {
		t.Fatalf("want 409, got %v", err)
	}
	if h.m.Lookup(heldID) != nil {
		t.Error("a refused prompt must not load the session")
	}
	select {
	case l := <-h.procs:
		t.Fatalf("launched %v", l.spec.Args)
	default:
	}
}

func TestForkContinuesInANewSession(t *testing.T) {
	h := heldHarness(t)
	id, err := h.m.Fork(heldID, "carry on")
	if err != nil {
		t.Fatal(err)
	}
	if id == heldID || id == "" {
		t.Fatalf("fork id %q", id)
	}
	h.sid = id
	l := h.waitLaunch()
	args := l.spec.Args
	i := slices.Index(args, "--resume")
	if i < 0 || args[i+1] != heldID || !slices.Contains(args, "--fork-session") {
		t.Fatalf("args %v", args)
	}
	if j := slices.Index(args, "--session-id"); j < 0 || args[j+1] != id {
		t.Fatalf("args %v", args)
	}
	if l.spec.Dir != h.cwd {
		t.Errorf("cwd %q, want the source's %q", l.spec.Dir, h.cwd)
	}
	if _, err := h.m.Fork("not-a-session", "x"); err == nil {
		t.Error("unknown source must fail")
	}
}

// holder starts a grandchild process (not "ours": its parent is a shell we
// started, not this server) to stand in for VS Code's claude.
func holder(t *testing.T) int {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "ping -n 60 127.0.0.1 >nul & rem")
	} else {
		cmd = exec.Command("sh", "-c", "sleep 60; :")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		for pid, parent := range parentPids() {
			if parent == cmd.Process.Pid {
				t.Cleanup(func() { _ = stopProcess(pid) })
				return pid
			}
		}
	}
	t.Skip("no child process listing on this platform")
	return 0
}

func TestReleaseClosesTheHolder(t *testing.T) {
	h := newHarness(t, Display{})
	dir := filepath.Dir(h.store.ProjectsDir())
	h.m.opts.ClaudeDir = dir
	pid := holder(t)
	registry(t, dir, pid, heldID, processStart(pid), "claude-vscode")
	// mid-turn: the last entry is the prompt
	writeTranscript(t, h.store, heldID, h.cwd, `{"type":"user","uuid":"u1","cwd":`+fmt.Sprintf("%q", h.cwd)+`,"message":{"role":"user","content":"hi"}}`)
	if _, held := h.m.HeldElsewhere(heldID); !held {
		t.Fatal("not seen as held")
	}
	var he *HTTPError
	if err := h.m.Release(heldID, false); !errors.As(err, &he) || he.Code != 409 {
		t.Fatalf("busy holder: want 409, got %v", err)
	}
	if !processAlive(pid) {
		t.Fatal("closed without force")
	}
	if err := h.m.Release(heldID, true); err != nil {
		t.Fatal(err)
	}
	if processAlive(pid) {
		t.Fatal("still running")
	}
	if _, held := h.m.HeldElsewhere(heldID); held {
		t.Fatal("still held")
	}
	// now prompts go through, in the same session
	if id, err := h.m.Prompt(heldID, "continue", nil); err != nil || id != heldID {
		t.Fatalf("%q %v", id, err)
	}
}

func TestReleaseIdleHolderNeedsNoForce(t *testing.T) {
	h := newHarness(t, Display{})
	dir := filepath.Dir(h.store.ProjectsDir())
	h.m.opts.ClaudeDir = dir
	pid := holder(t)
	registry(t, dir, pid, heldID, processStart(pid), "cli")
	writeTranscript(t, h.store, heldID, h.cwd,
		`{"type":"user","uuid":"u1","cwd":`+fmt.Sprintf("%q", h.cwd)+`,"message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"hello"}]}}`)
	if err := h.m.Release(heldID, false); err != nil {
		t.Fatal(err)
	}
	if processAlive(pid) {
		t.Fatal("still running")
	}
	if err := h.m.Release(heldID, false); err != nil {
		t.Fatal("nobody holds it: nothing to do", err)
	}
}
