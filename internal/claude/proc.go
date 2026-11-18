package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"even-cc/internal/guard"
	"even-cc/internal/jsonx"
)

// procTree stops claude together with everything it started (proc_unix.go,
// proc_windows.go).
type procTree interface {
	terminate() // polite where the OS has it (SIGTERM to the group)
	kill()
	release() // after exit: end stragglers, free OS resources
}

// Proc is one long-lived `claude` process in stream-json mode.
type Proc struct {
	log   *slog.Logger
	cmd   *exec.Cmd
	tree  procTree
	stdin io.WriteCloser

	wq       chan []byte // nil entry = close stdin
	done     chan struct{}
	exitCode int
	exitErr  error

	mu          sync.Mutex
	pending     map[string]chan controlResult
	nextReq     int
	dead        bool
	closing     bool   // Close called: stdin is (being) closed
	stderr      []byte // tail
	stderrLines int
}

// stderrLogLines: the first stderr lines of a process are logged at info,
// the rest only at debug (a chatty CLI can't flood the log).
const stderrLogLines = 50

// sendTimeout bounds how long Send waits for room in the write queue; the
// caller usually holds its session's lock.
const sendTimeout = 5 * time.Second

// Close's stages (variables for tests): wait after stdin EOF, after
// terminate, and after kill.
var (
	closeEOFWait       = 2 * time.Second
	closeTerminateWait = 5 * time.Second
	closeKillWait      = 5 * time.Second
)

type controlResult struct {
	ok       bool
	response json.RawMessage
	err      string
}

// ProcSpec describes how to launch claude.
type ProcSpec struct {
	Path string
	Args []string
	Dir  string
	Env  []string
	Log  *slog.Logger // the session's logger; nil = the manager's
}

// StartProc spawns claude. onLine gets every non-control-response stdout
// line (serially, from one goroutine); onExit is called once after exit.
func StartProc(spec ProcSpec, log *slog.Logger, onLine func(line []byte), onExit func(p *Proc)) (*Proc, error) {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	setProcAttrs(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Proc{
		log:     log,
		cmd:     cmd,
		tree:    newProcTree(cmd, log),
		stdin:   stdin,
		wq:      make(chan []byte, 1024),
		done:    make(chan struct{}),
		pending: map[string]chan controlResult{},
	}
	go func() {
		defer guard.Recover(log, "cli stdin writer")
		p.writer()
	}()
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		defer guard.Recover(log, "cli stderr reader")
		p.readStderr(stderr)
	}()
	go func() {
		// Exit bookkeeping runs whatever happens to the reader, so Close and
		// the session always learn that the process is gone.
		defer p.finish(stderrDone, onExit)
		ok := false
		func() {
			defer guard.Recover(log, "cli stdout reader")
			p.readStdout(stdout, onLine)
			ok = true
		}()
		if !ok {
			log.Error("stdout reader failed; killing claude", "pid", p.Pid())
			p.tree.kill()
		}
	}()
	return p, nil
}

// finish waits for the exit, fails pending requests and reports the exit.
func (p *Proc) finish(stderrDone <-chan struct{}, onExit func(*Proc)) {
	defer guard.Recover(p.log, "cli exit")
	err := p.cmd.Wait()
	select {
	case <-stderrDone:
	case <-time.After(200 * time.Millisecond):
	}
	p.tree.release()
	p.mu.Lock()
	p.dead = true
	p.exitErr = err
	p.exitCode = p.cmd.ProcessState.ExitCode()
	for id, ch := range p.pending {
		ch <- controlResult{err: "claude process exited"}
		delete(p.pending, id)
	}
	p.mu.Unlock()
	close(p.done)
	if code := p.ExitCode(); code == 0 {
		p.log.Info("claude exited", "pid", p.Pid(), "code", code)
	} else {
		p.log.Warn("claude exited", "pid", p.Pid(), "code", code, "detail", p.ExitDescription())
	}
	onExit(p)
}

// Pid of the process.
func (p *Proc) Pid() int { return p.cmd.Process.Pid }

// Done is closed after the process exited.
func (p *Proc) Done() <-chan struct{} { return p.done }

// ExitDescription is like the SDK's exit error text.
func (p *Proc) ExitDescription() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	tail := strings.TrimSpace(string(p.stderr))
	if r := []rune(tail); len(r) > 300 {
		tail = "…" + string(r[len(r)-300:])
	}
	msg := fmt.Sprintf("Claude Code process exited with code %d", p.exitCode)
	if p.exitCode < 0 && p.exitErr != nil {
		msg = "Claude Code process terminated: " + p.exitErr.Error()
	}
	if tail != "" {
		msg += ". stderr: " + tail
	}
	return msg
}

// ExitCode after Done.
func (p *Proc) ExitCode() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode
}

// writer owns stdin: one complete line per write, until close or exit.
func (p *Proc) writer() {
	for {
		select {
		case b := <-p.wq:
			if b == nil {
				if err := p.stdin.Close(); err != nil {
					p.log.Debug("stdin close failed", "pid", p.Pid(), "err", err)
				}
				return
			}
			if _, err := p.stdin.Write(b); err != nil {
				p.log.Warn("stdin write failed; line lost", "pid", p.Pid(), "err", err, "line", jsonx.TruncateBytes(string(b), 200))
			}
		case <-p.done:
			return
		}
	}
}

// Send queues one message as a single complete JSON line. A message that
// can't be delivered (process exiting, queue stuck) is logged and dropped.
func (p *Proc) Send(v any) {
	b, err := jsonx.Encode(v)
	if err != nil {
		p.log.Error("cli message not sent: encoding failed", "pid", p.Pid(), "err", err)
		return
	}
	b = append(b, '\n')
	p.mu.Lock()
	dead, closing := p.dead, p.closing
	p.mu.Unlock()
	if dead || closing {
		p.log.Warn("cli message dropped: process is exiting", "pid", p.Pid(), "line", jsonx.TruncateBytes(string(b), 200))
		return
	}
	if p.log.Enabled(context.Background(), slog.LevelDebug) {
		p.log.Debug("cli <-", "pid", p.Pid(), "line", jsonx.TruncateBytes(string(b), 2000))
	}
	select {
	case p.wq <- b:
	case <-p.done:
		p.log.Warn("cli message dropped: process exited", "pid", p.Pid(), "line", jsonx.TruncateBytes(string(b), 200))
	case <-time.After(sendTimeout):
		p.log.Error("cli message dropped: claude is not reading stdin", "pid", p.Pid(), "line", jsonx.TruncateBytes(string(b), 200))
	}
}

// StartRequest queues a host->CLI control request (in order with other
// writes) and returns a function that waits for its response.
func (p *Proc) StartRequest(subtype string, fields map[string]any) func(timeout time.Duration) (json.RawMessage, error) {
	p.mu.Lock()
	if p.dead {
		p.mu.Unlock()
		return func(time.Duration) (json.RawMessage, error) {
			return nil, errors.New("claude process is not running")
		}
	}
	p.nextReq++
	id := "ecc_" + strconv.Itoa(p.nextReq)
	ch := make(chan controlResult, 1)
	p.pending[id] = ch
	p.mu.Unlock()

	req := map[string]any{"subtype": subtype}
	for k, v := range fields {
		req[k] = v
	}
	p.Send(map[string]any{"type": "control_request", "request_id": id, "request": req})
	return func(timeout time.Duration) (json.RawMessage, error) {
		select {
		case r := <-ch:
			if !r.ok {
				return nil, errors.New(r.err)
			}
			return r.response, nil
		case <-time.After(timeout):
			p.mu.Lock()
			delete(p.pending, id)
			p.mu.Unlock()
			return nil, fmt.Errorf("%s: no response within %s", subtype, timeout)
		}
	}
}

// Request sends a host->CLI control request and waits for its response.
func (p *Proc) Request(subtype string, fields map[string]any, timeout time.Duration) (json.RawMessage, error) {
	return p.StartRequest(subtype, fields)(timeout)
}

// Respond answers a CLI->host control request.
func (p *Proc) Respond(requestID string, response any) {
	p.Send(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": requestID, "response": response,
	}})
}

// RespondError answers a CLI->host control request with an error.
func (p *Proc) RespondError(requestID, msg string) {
	p.Send(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "error", "request_id": requestID, "error": msg,
	}})
}

// Close ends the process and everything it started: stdin EOF, then
// terminate after 2 s, then kill after 5 s more. It returns once the process
// is gone, or after a last 5 s wait if even the kill didn't end it (logged).
// Commands queued after Close are dropped (logged).
func (p *Proc) Close() {
	p.mu.Lock()
	dead := p.dead
	p.closing = true
	p.mu.Unlock()
	if !dead {
		select {
		case p.wq <- nil:
		case <-p.done:
			return
		case <-time.After(time.Second):
			p.log.Warn("stdin queue stuck; terminating claude", "pid", p.Pid())
		}
	}
	if p.waitDone(closeEOFWait) {
		return
	}
	p.log.Info("claude still running after stdin EOF; terminating", "pid", p.Pid())
	p.tree.terminate()
	if p.waitDone(closeTerminateWait) {
		return
	}
	p.log.Warn("claude ignored terminate; killing its process tree", "pid", p.Pid())
	p.tree.kill()
	if !p.waitDone(closeKillWait) {
		p.log.Error("claude did not exit after kill; giving up on it", "pid", p.Pid())
	}
}

func (p *Proc) waitDone(d time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(d):
		return false
	}
}

func (p *Proc) readStderr(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			p.mu.Lock()
			p.stderr = append(p.stderr, line...)
			if len(p.stderr) > 4096 {
				p.stderr = append([]byte(nil), p.stderr[len(p.stderr)-4096:]...)
			}
			n := 0
			if t := strings.TrimSpace(string(line)); t != "" {
				p.stderrLines++
				n = p.stderrLines
			}
			p.mu.Unlock()
			if n > 0 {
				t := jsonx.TruncateBytes(strings.TrimSpace(string(line)), 500)
				switch {
				case n < stderrLogLines:
					p.log.Info("cli stderr", "pid", p.Pid(), "line", t)
				case n == stderrLogLines:
					p.log.Info("cli stderr (further lines only at debug level)", "pid", p.Pid(), "line", t)
				default:
					p.log.Debug("cli stderr", "pid", p.Pid(), "line", t)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

type envelope struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Response  *struct {
		Subtype   string          `json:"subtype"`
		RequestID string          `json:"request_id"`
		Response  json.RawMessage `json:"response"`
		Error     string          `json:"error"`
	} `json:"response"`
}

func (p *Proc) readStdout(r io.Reader, onLine func([]byte)) {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			p.handleLine(line, onLine)
		}
		if err != nil {
			return
		}
	}
}

func (p *Proc) handleLine(line []byte, onLine func([]byte)) {
	if line[0] != '{' {
		p.log.Debug("cli non-json stdout", "line", jsonx.TruncateBytes(string(line), 300))
		return
	}
	if p.log.Enabled(context.Background(), slog.LevelDebug) {
		p.log.Debug("cli ->", "pid", p.Pid(), "line", jsonx.TruncateBytes(string(line), 2000))
	}
	if bytes.Contains(line, []byte(`"control_response"`)) {
		var env envelope
		if json.Unmarshal(line, &env) == nil && env.Type == "control_response" && env.Response != nil {
			p.mu.Lock()
			ch := p.pending[env.Response.RequestID]
			delete(p.pending, env.Response.RequestID)
			p.mu.Unlock()
			if ch != nil {
				ch <- controlResult{ok: env.Response.Subtype == "success", response: env.Response.Response, err: env.Response.Error}
			}
			return
		}
	}
	onLine(line)
}

// FindClaude resolves the claude executable: explicit path, else PATH. On
// Windows an npm .cmd shim is resolved to the claude.exe it runs.
func FindClaude(configured string) (string, error) {
	if configured != "" {
		if _, err := os.Stat(configured); err == nil {
			return resolveShim(configured), nil
		}
		if p, err := exec.LookPath(configured); err == nil {
			return resolveShim(p), nil
		}
		return "", fmt.Errorf("claude executable %q not found", configured)
	}
	for _, name := range []string{"claude", "claude.exe", "claude.cmd"} {
		if p, err := exec.LookPath(name); err == nil {
			return resolveShim(p), nil
		}
	}
	return "", errors.New("claude not found on PATH; set it with: even-cc config claude.path <path>")
}

// scrubbedEnvKeys are dropped from the child's env so a host started from
// inside Claude Code doesn't leak its session into the child.
var scrubbedEnvKeys = map[string]bool{
	"CLAUDECODE": true, "CLAUDE_CODE_SESSION_ID": true, "CLAUDE_CODE_CHILD_SESSION": true,
	"CLAUDE_PID": true, "CLAUDE_CODE_MESSAGING_SOCKET": true, "CLAUDE_CODE_MESSAGING_TOKEN": true,
	"CLAUDE_CODE_ENTRYPOINT": true, "CLAUDE_EFFORT": true, "AI_AGENT": true, "NODE_OPTIONS": true,
	"DEBUG": true, "CLAUDE_CODE_SSE_PORT": true, "CLAUDE_AGENT_SDK_VERSION": true,
	"CLAUDE_CODE_SDK_READS_SESSION_STATE": true, "CLAUDE_CODE_PARENT_SESSION_ID": true,
	"CLAUDE_CODE_TASK_ID": true, "CLAUDE_CODE_AGENT_ID": true,
}

// ScrubEnv drops the variables that would tie a child claude to the
// Claude Code session even-cc itself may run under.
func ScrubEnv(base []string) []string {
	out := make([]string, 0, len(base)+3)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if scrubbedEnvKeys[strings.ToUpper(k)] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// ChildEnv builds the claude process environment.
func ChildEnv(base []string) []string {
	return append(ScrubEnv(base),
		"CLAUDE_CODE_ENTRYPOINT=sdk-ts",
		"CLAUDE_CODE_SDK_READS_SESSION_STATE=1",
	)
}
