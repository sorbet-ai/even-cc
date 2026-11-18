package claude

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as a fake claude (EVENCC_FAKE), so Proc is tested
// against real processes, pipes and process trees.
func TestMain(m *testing.M) {
	switch os.Getenv("EVENCC_FAKE") {
	case "stubborn":
		fakeStubborn()
		return
	case "holder":
		time.Sleep(time.Hour)
		return
	case "echo":
		fakeEcho()
		return
	}
	os.Exit(m.Run())
}

// fakeStubborn starts a grandchild that shares its stdout (like an MCP
// server or a node.exe behind a .cmd shim), then ignores stdin EOF and
// SIGTERM.
func fakeStubborn() {
	signal.Ignore(syscall.SIGTERM)
	gc := exec.Command(os.Args[0])
	gc.Env = append(os.Environ(), "EVENCC_FAKE=holder")
	gc.Stdout = os.Stdout
	if err := gc.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "grandchild:", err)
		os.Exit(3)
	}
	_ = os.WriteFile(os.Getenv("EVENCC_PIDFILE"), []byte(strconv.Itoa(gc.Process.Pid)), 0o600)
	fmt.Fprintln(os.Stderr, "fake stderr line")
	fmt.Println(`{"type":"system","subtype":"init"}`)
	_, _ = bufio.NewReader(os.Stdin).ReadString(0) // until EOF
	time.Sleep(time.Hour)
}

func fakeEcho() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		fmt.Println(sc.Text())
	}
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func fakeSpec(t *testing.T, mode string, extraEnv ...string) ProcSpec {
	// A -race test binary sleeps 1 s before exiting unless told not to.
	env := append(os.Environ(), "EVENCC_FAKE="+mode, "GORACE=atexit_sleep_ms=0")
	return ProcSpec{Path: os.Args[0], Dir: t.TempDir(), Env: append(env, extraEnv...)}
}

func TestProcCloseKillsWholeTree(t *testing.T) {
	defer func(a, b, c time.Duration) { closeEOFWait, closeTerminateWait, closeKillWait = a, b, c }(closeEOFWait, closeTerminateWait, closeKillWait)
	closeEOFWait, closeTerminateWait, closeKillWait = 200*time.Millisecond, 500*time.Millisecond, 5*time.Second

	pidFile := filepath.Join(t.TempDir(), "gc.pid")
	var logs logBuf
	log := slog.New(slog.NewTextHandler(&logs, nil))
	lines := make(chan string, 8)
	exited := make(chan struct{})
	p, err := StartProc(fakeSpec(t, "stubborn", "EVENCC_PIDFILE="+pidFile), log,
		func(l []byte) { lines <- string(l) }, func(*Proc) { close(exited) })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case l := <-lines:
		if !strings.Contains(l, `"init"`) {
			t.Fatalf("line %s", l)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no output from the fake claude")
	}
	var gc int
	for deadline := time.Now().Add(5 * time.Second); gc == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		b, _ := os.ReadFile(pidFile)
		gc, _ = strconv.Atoi(string(b))
	}
	if gc == 0 || !processAlive(gc) {
		t.Fatalf("grandchild not running (pid %d)", gc)
	}

	start := time.Now()
	p.Close() // the grandchild holds stdout open: only a tree kill ends this
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("Close took %s", d)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("onExit not called")
	}
	for deadline := time.Now().Add(3 * time.Second); processAlive(gc) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(gc) {
		t.Errorf("grandchild %d survived Close", gc)
	}
	if out := logs.String(); !strings.Contains(out, "fake stderr line") || !strings.Contains(out, "level=INFO msg=\"cli stderr\"") {
		t.Errorf("stderr not logged at info:\n%s", out)
	}
	p.Send(map[string]string{"type": "user"}) // after exit: dropped and logged, no block or panic
	if !strings.Contains(logs.String(), "cli message dropped") {
		t.Error("send after exit not logged")
	}
}

func TestProcEchoAndGracefulClose(t *testing.T) {
	var logs logBuf
	log := slog.New(slog.NewTextHandler(&logs, nil))
	lines := make(chan string, 8)
	p, err := StartProc(fakeSpec(t, "echo"), log, func(l []byte) { lines <- string(l) }, func(*Proc) {})
	if err != nil {
		t.Fatal(err)
	}
	p.Send(map[string]string{"type": "user", "text": "hi"})
	select {
	case l := <-lines:
		if l != `{"text":"hi","type":"user"}` {
			t.Errorf("echo %s", l)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no echo")
	}
	start := time.Now()
	p.Close() // exits on stdin EOF
	if d := time.Since(start); d > time.Second {
		t.Errorf("graceful Close took %s", d)
	}
	if p.ExitCode() != 0 {
		t.Errorf("exit %d: %s", p.ExitCode(), p.ExitDescription())
	}
	p.Send(map[string]string{"type": "user"})
	if !strings.Contains(logs.String(), "cli message dropped") {
		t.Errorf("send after close not logged:\n%s", logs.String())
	}
}
