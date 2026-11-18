package claude

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"even-cc/internal/hub"
	"even-cc/internal/jsonx"
	"even-cc/internal/transcript"
)

// fakeProc records what the session writes to the CLI.
type fakeProc struct {
	mu     sync.Mutex
	sent   []string
	reqs   []string
	fields []map[string]any
	closed bool
	code   int
}

func (f *fakeProc) Send(v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, string(jsonx.Marshal(v)))
}
func (f *fakeProc) StartRequest(subtype string, fields map[string]any) func(time.Duration) (json.RawMessage, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, subtype)
	f.fields = append(f.fields, fields)
	f.mu.Unlock()
	return func(time.Duration) (json.RawMessage, error) {
		switch subtype {
		case "set_permission_mode":
			return jsonx.Marshal(map[string]any{"mode": fields["mode"]}), nil
		case "set_model":
			return json.RawMessage(`{"model":"claude-sonnet-5-5"}`), nil
		}
		return json.RawMessage(`{}`), nil
	}
}
func (f *fakeProc) Respond(id string, r any) {
	f.Send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": r}})
}
func (f *fakeProc) RespondError(id, msg string) {
	f.Send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": msg}})
}
func (f *fakeProc) Close()                  { f.mu.Lock(); f.closed = true; f.mu.Unlock() }
func (f *fakeProc) Pid() int                { return 4242 }
func (f *fakeProc) ExitDescription() string { return "Claude Code process exited with code 1" }
func (f *fakeProc) ExitCode() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.code == 0 {
		return 1
	}
	if f.code == -100 {
		return 0
	}
	return f.code
}
func (f *fakeProc) sentLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

type launched struct {
	proc   *fakeProc
	spec   ProcSpec
	onLine func([]byte)
	onExit func(procHandle)
}

type harness struct {
	t     *testing.T
	m     *Manager
	hub   *hub.Hub
	store *transcript.Store
	cwd   string
	procs chan *launched
	cur   *launched
	sid   string
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newHarness(t *testing.T, d Display) *harness {
	t.Helper()
	claudeDir := t.TempDir()
	cwd := t.TempDir()
	h := &harness{t: t, hub: hub.New(nil), store: transcript.NewStore(claudeDir), cwd: cwd, procs: make(chan *launched, 8)}
	h.m = NewManager(Options{ClaudePath: "claude", DefaultCwd: cwd, StatsInterval: -1, AgentTick: -1, ResultIdleGrace: 50 * time.Millisecond, SettingSources: "user,project,local"}, d, h.hub, h.store, quietLog())
	h.m.launch = func(spec ProcSpec, onLine func([]byte), onExit func(procHandle)) (procHandle, error) {
		p := &fakeProc{}
		h.procs <- &launched{proc: p, spec: spec, onLine: onLine, onExit: onExit}
		return p, nil
	}
	return h
}

func (h *harness) prompt(text string) {
	h.t.Helper()
	id, err := h.m.Prompt(h.sid, text, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	h.sid = id
}

// waitLaunch waits for the session to spawn a (fake) process.
func (h *harness) waitLaunch() *launched {
	h.t.Helper()
	select {
	case l := <-h.procs:
		h.cur = l
		// launchThenSend sends the user line right after launch; wait for it.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			for _, s := range l.proc.sentLines() {
				if strings.Contains(s, `"type":"user"`) {
					return l
				}
			}
			time.Sleep(time.Millisecond)
		}
		h.t.Fatal("user message never sent")
	case <-time.After(2 * time.Second):
		h.t.Fatal("no process launched")
	}
	return nil
}

func (h *harness) feed(line []byte) { h.cur.onLine(line) }

// rawEvents returns the session's buffered events, as a replaying client
// gets them.
func (h *harness) rawEvents() [][]byte {
	c := h.hub.Subscribe(h.sid, true)
	defer h.hub.Unsubscribe(h.sid, c)
	out := make([][]byte, 0, len(c.C))
	for n := len(c.C); n > 0; n-- {
		out = append(out, <-c.C)
	}
	return out
}

// events returns all events emitted for the session.
func (h *harness) events() []map[string]any {
	var list []map[string]any
	for _, b := range h.rawEvents() {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		list = append(list, m)
	}
	return list
}

// signature renders events compactly: type[:state|name|decision], with runs
// of text_delta collapsed.
func signature(evs []map[string]any) []string {
	var out []string
	for _, e := range evs {
		t, _ := e["type"].(string)
		s := t
		switch t {
		case "status":
			s = e["state"].(string)
		case "tool_start", "tool_end":
			s = t + ":" + e["name"].(string)
		case "permission_result":
			s = t + ":" + e["decision"].(string)
		case "result":
			if e["success"] == true {
				s = "result:ok"
			} else {
				s = "result:fail"
			}
		case "text_delta":
			s = "text_delta+"
			if len(out) > 0 && out[len(out)-1] == s {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

type fixtureRec struct {
	T    float64         `json:"t"`
	Dir  string          `json:"dir"`
	Data json.RawMessage `json:"data"`
}

func loadFixture(t *testing.T, name string) []fixtureRec {
	var out []fixtureRec
	for _, l := range readLines(t, filepath.Join(testdata, "claude", name+".jsonl")) {
		var r fixtureRec
		if err := json.Unmarshal(l, &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// replay drives a session through a recorded CLI run: recv lines are fed as
// stdout; the probe's own user prompts / answers / interrupts become the
// equivalent app actions.
func (h *harness) replay(name string, answer func(req map[string]any, resp map[string]any)) {
	h.t.Helper()
	recs := loadFixture(h.t, name)
	first := true
	for _, r := range recs {
		var d map[string]any
		_ = json.Unmarshal(r.Data, &d)
		switch r.Dir {
		case "send":
			switch d["type"] {
			case "user":
				msg := d["message"].(map[string]any)
				h.prompt(msg["content"].(string))
				if first {
					h.waitLaunch()
					first = false
				}
			case "control_request":
				req := d["request"].(map[string]any)
				if req["subtype"] == "interrupt" {
					h.m.Lookup(h.sid).Interrupt()
				}
			case "control_response":
				resp := d["response"].(map[string]any)
				if answer != nil {
					answer(nil, resp)
				}
			}
		case "recv":
			if d["type"] == "control_response" {
				continue // replies to the probe's own requests
			}
			h.feed(r.Data)
		case "exit":
			h.cur.proc.mu.Lock()
			h.cur.proc.code = int(d["code"].(float64))
			if h.cur.proc.code == 0 {
				h.cur.proc.code = -100 // sentinel: report 0
			}
			h.cur.proc.mu.Unlock()
			h.cur.onExit(h.cur.proc)
		}
	}
}

func wantSig(t *testing.T, got []string, want string) {
	t.Helper()
	if g := strings.Join(got, " "); g != want {
		t.Errorf("event sequence\n got: %s\nwant: %s", g, want)
	}
}

func lastOf(evs []map[string]any, typ string) map[string]any {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i]["type"] == typ {
			return evs[i]
		}
	}
	return nil
}

func TestReplayMultiturnWarmReuse(t *testing.T) {
	h := newHarness(t, ClassicDisplay())
	h.replay("multiturn", nil)
	evs := h.events()
	wantSig(t, signature(evs), "user_prompt busy text_start text_delta+ text_end result:ok idle user_prompt busy text_start text_delta+ text_end result:ok idle")
	// one process for both prompts; second prompt sent on the same stdin
	users := 0
	for _, s := range h.cur.proc.sentLines() {
		if strings.Contains(s, `"type":"user"`) {
			users++
		}
	}
	if users != 2 {
		t.Errorf("want 2 user messages on one process, got %d", users)
	}
	select {
	case <-h.procs:
		t.Error("second process launched")
	default:
	}
	// per-turn cost/tokens (CLI totals are cumulative)
	var results []map[string]any
	for _, e := range evs {
		if e["type"] == "result" {
			results = append(results, e)
		}
	}
	if results[0]["costUsd"].(float64) != 0.00159347 || results[1]["costUsd"].(float64) != 0.0015288800000000003 {
		t.Errorf("per-turn costs %v %v", results[0]["costUsd"], results[1]["costUsd"])
	}
	if results[1]["inputTokens"].(float64) != 2 || results[1]["outputTokens"].(float64) != 4 || results[1]["turns"].(float64) != 1 {
		t.Errorf("turn 2 tokens %v", results[1])
	}
	if results[1]["text"] != "OK" {
		t.Errorf("text %v", results[1]["text"])
	}
	// launch flags
	spec := h.cur.spec
	for _, want := range []string{"--input-format", "stream-json", "--permission-prompt-tool", "stdio", "--setting-sources", "user,project,local", "--session-id", h.sid, "--include-partial-messages"} {
		if !hasArg(spec.Args, want) {
			t.Errorf("missing arg %q in %v", want, spec.Args)
		}
	}
	env := strings.Join(spec.Env, "\n")
	if !strings.Contains(env, "CLAUDE_CODE_SDK_READS_SESSION_STATE=1") || strings.Contains(env, "CLAUDECODE=") {
		t.Errorf("bad env")
	}
	if h.m.StatusOf(h.sid) != "idle" {
		t.Errorf("state %s", h.m.StatusOf(h.sid))
	}
}

func TestReplayPermissionDenyThenAlways(t *testing.T) {
	h := newHarness(t, ClassicDisplay())
	n := 0
	h.replay("permission", func(_ map[string]any, resp map[string]any) {
		inner, _ := resp["response"].(map[string]any)
		if inner == nil || inner["behavior"] == nil {
			return // hook reply
		}
		s := h.m.Lookup(h.sid)
		n++
		var ok bool
		switch {
		case inner["behavior"] == "deny":
			ok = s.RespondPermission("deny")
		case inner["updatedPermissions"] != nil:
			ok = s.RespondPermission("allowAlways")
		default:
			ok = s.RespondPermission("allow")
		}
		if !ok {
			t.Errorf("permission answer %d rejected", n)
		}
	})
	evs := h.events()
	wantSig(t, signature(evs), strings.Join([]string{
		"user_prompt busy tool_start:Write permission_request permission_result:denied tool_end:Write think_start think_end text_start text_delta+ text_end result:ok idle",
		"user_prompt busy tool_start:Write permission_request permission_result:always tool_end:Write text_start text_delta+ text_end result:ok idle",
		"user_prompt busy tool_start:Write tool_end:Write text_start text_delta+ text_end result:ok idle",
	}, " "))
	var req map[string]any
	for _, e := range evs {
		if e["type"] == "permission_request" {
			req = e
			break
		}
	}
	if req["description"] != "Write x.txt (1 lines)" || req["detail"] != "/home/user/even-cc/.test/cli-probe/work/x.txt" {
		t.Errorf("permission_request %v", req)
	}
	// replies to the CLI
	var deny, always string
	for _, s := range h.cur.proc.sentLines() {
		if strings.Contains(s, `"behavior":"deny"`) {
			deny = s
		}
		if strings.Contains(s, `"updatedPermissions"`) {
			always = s
		}
	}
	if !strings.Contains(deny, `"message":"Denied by user"`) || !strings.Contains(deny, `"request_id":"72de1d4c-0ae2-474f-8951-07ed5c58670b"`) {
		t.Errorf("deny reply %s", deny)
	}
	if !strings.Contains(always, `"updatedPermissions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}]`) || !strings.Contains(always, `"updatedInput":{"file_path"`) {
		t.Errorf("always reply %s", always)
	}
	// unanswered permission request: not offered decisions are rejected
	if h.m.Lookup(h.sid).RespondPermission("allow") {
		t.Error("answer with nothing pending accepted")
	}
}

func TestReplayAskUserQuestion(t *testing.T) {
	h := newHarness(t, ClassicDisplay())
	h.replay("askuser", func(_ map[string]any, resp map[string]any) {
		inner, _ := resp["response"].(map[string]any)
		if inner == nil || inner["behavior"] == nil {
			return
		}
		h.m.Lookup(h.sid).RespondQuestion(`{"Pick a color?":"Green"}`)
	})
	evs := h.events()
	wantSig(t, signature(evs), "user_prompt busy tool_start:AskUserQuestion user_question question_answer tool_end:AskUserQuestion think_start think_end text_start text_delta+ text_end result:ok idle")
	q := lastOf(evs, "user_question")
	if got := string(jsonx.Marshal(q["questions"])); got != `[{"header":"Color","multiSelect":false,"options":[{"description":"Pick red","label":"Red","preview":""},{"description":"Pick green","label":"Green","preview":""},{"description":"Pick blue","label":"Blue","preview":""}],"question":"Pick a color?"}]` {
		t.Errorf("questions %s", got)
	}
	raw := bytes.Join(h.rawEvents(), nil)
	if !strings.Contains(string(raw), `{"type":"question_answer","answers":{"Pick a color?":"Green"}}`) {
		t.Errorf("question_answer missing")
	}
	var reply string
	for _, s := range h.cur.proc.sentLines() {
		if strings.Contains(s, `"answers"`) {
			reply = s
		}
	}
	if !strings.Contains(reply, `"answers":{"Pick a color?":"Green"}`) || !strings.Contains(reply, `"questions":[{"question":"Pick a color?"`) {
		t.Errorf("reply %s", reply)
	}
}

func TestAnswersEncoding(t *testing.T) {
	qs := normalizeQuestions(json.RawMessage(`[{"question":"Q1","options":[]},{"header":"H2"},{"question":"Q1"}]`))
	if got := string(answersFor("Choice A", qs)); got != `{"Q1":"Choice A","H2":"Choice A"}` {
		t.Errorf("raw answer: %s", got)
	}
	if got := string(answersFor(`{"Q1":"A, B"}`, qs)); got != `{"Q1":"A, B"}` {
		t.Errorf("json answer: %s", got)
	}
	if got := string(answersFor("1", qs)); got != `1` {
		t.Errorf("json scalar: %s", got)
	}
}

func TestReplayInterrupt(t *testing.T) {
	h := newHarness(t, ClassicDisplay())
	h.replay("interrupt", nil)
	evs := h.events()
	wantSig(t, signature(evs), "user_prompt busy text_start text_delta+ text_end result:fail idle user_prompt busy text_start text_delta+ text_end result:ok idle")
	var first map[string]any
	for _, e := range evs {
		if e["type"] == "result" {
			first = e
			break
		}
	}
	if first["text"] != "Interrupted by user" {
		t.Errorf("interrupt text %v", first["text"])
	}
	interrupts := 0
	h.cur.proc.mu.Lock()
	for _, r := range h.cur.proc.reqs {
		if r == "interrupt" {
			interrupts++
		}
	}
	h.cur.proc.mu.Unlock()
	if interrupts != 1 {
		t.Errorf("interrupt requests: %d", interrupts)
	}
	// interrupt is not a failure: no recycle
	s := h.m.Lookup(h.sid)
	if s.recycleNext {
		t.Error("interrupt must not recycle the process")
	}
}

func TestReplayMinimalWithoutSessionState(t *testing.T) {
	h := newHarness(t, ClassicDisplay())
	h.replay("minimal", nil)
	wantSig(t, signature(h.events()), "user_prompt busy text_start text_delta+ text_end result:ok idle")
}

func TestReplayEOFAndKillMidTurn(t *testing.T) {
	for _, name := range []string{"eofmidturn", "killmidturn"} {
		h := newHarness(t, ClassicDisplay())
		h.replay(name, nil)
		sig := strings.Join(signature(h.events()), " ")
		if !strings.HasPrefix(sig, "user_prompt busy") || !strings.HasSuffix(sig, "result:ok idle") {
			t.Errorf("%s: %s", name, sig)
		}
	}
}

func TestProcessDeathMidTurnEmitsErrorAndIdle(t *testing.T) {
	h := newHarness(t, ClassicDisplay())
	h.prompt("hello")
	l := h.waitLaunch()
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"running","session_id":"x"}`))
	l.onExit(l.proc)
	wantSig(t, signature(h.events()), "user_prompt busy error idle")
	if e := lastOf(h.events(), "error"); !strings.Contains(e["message"].(string), "exited with code 1") {
		t.Errorf("error %v", e)
	}
	// next prompt launches a fresh process
	h.prompt("again")
	h.waitLaunch()
}

// A prompt sent while a turn runs goes straight to the CLI's stdin, which
// folds it into the running turn (probe "midturn", 2.1.312): one result, one
// idle.
func TestPromptMidTurnWrittenThrough(t *testing.T) {
	h := newHarness(t, ClassicDisplay())
	h.prompt("one")
	l := h.waitLaunch()
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"running","session_id":"x"}`))
	h.prompt("two")
	if st := h.m.StatusOf(h.sid); st != "busy" {
		t.Fatalf("state %s", st)
	}
	var users []string
	for _, s := range l.proc.sentLines() {
		if strings.Contains(s, `"type":"user"`) {
			users = append(users, s)
		}
	}
	if len(users) != 2 || !strings.Contains(users[1], `"content":"two"`) {
		t.Fatalf("second prompt not written through: %v", users)
	}
	h.feed([]byte(`{"type":"result","subtype":"success","result":"A\nB","num_turns":2,"session_id":"x","total_cost_usd":0.5}`))
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"idle","session_id":"x"}`))
	wantSig(t, signature(h.events()), "user_prompt busy user_prompt result:ok idle")
}

// Sequential turns: per-turn cost from the CLI's cumulative totals when
// model_usage_since_last_result is absent.
func TestPerTurnCostFallback(t *testing.T) {
	h := newHarness(t, ClassicDisplay())
	h.prompt("one")
	h.waitLaunch()
	running := []byte(`{"type":"system","subtype":"session_state_changed","state":"running","session_id":"x"}`)
	idle := []byte(`{"type":"system","subtype":"session_state_changed","state":"idle","session_id":"x"}`)
	h.feed(running)
	h.feed([]byte(`{"type":"result","subtype":"success","result":"r","num_turns":1,"session_id":"x","total_cost_usd":0.5}`))
	h.feed(idle)
	h.prompt("two")
	h.feed(running)
	h.feed([]byte(`{"type":"result","subtype":"success","result":"r2","num_turns":1,"session_id":"x","total_cost_usd":0.75}`))
	h.feed(idle)
	wantSig(t, signature(h.events()), "user_prompt busy result:ok idle user_prompt busy result:ok idle")
	var costs []float64
	for _, e := range h.events() {
		if e["type"] == "result" {
			costs = append(costs, e["costUsd"].(float64))
		}
	}
	if costs[0] != 0.5 || costs[1] != 0.25 {
		t.Errorf("costs %v", costs)
	}
}

func TestStrayEmptyResultDropped(t *testing.T) {
	h := newHarness(t, ClassicDisplay())
	h.prompt("one")
	h.waitLaunch()
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"running"}`))
	h.feed([]byte(`{"type":"result","subtype":"success","result":"","num_turns":0,"session_id":"x"}`))
	h.feed([]byte(`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}},"parent_tool_use_id":null}`))
	h.feed([]byte(`{"type":"stream_event","event":{"type":"content_block_stop","index":0},"parent_tool_use_id":null}`))
	h.feed([]byte(`{"type":"result","subtype":"success","result":"real","num_turns":1,"session_id":"x"}`))
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"idle"}`))
	wantSig(t, signature(h.events()), "user_prompt busy text_start text_end result:ok idle")
}

// Background task finishing → Claude starts its own turn; it's tracked like
// a prompt (busy → events → result → idle) and keeps the process.
func TestBackgroundTaskAutonomousTurn(t *testing.T) {
	h := newHarness(t, ClassicDisplay())
	h.prompt("start bg")
	l := h.waitLaunch()
	for _, line := range []string{
		`{"type":"system","subtype":"session_state_changed","state":"running"}`,
		`{"type":"system","subtype":"task_started","task_id":"b1","tool_use_id":"t1"}`,
		`{"type":"result","subtype":"success","result":"started","num_turns":2,"session_id":"x"}`,
		`{"type":"system","subtype":"session_state_changed","state":"idle"}`,
	} {
		h.feed([]byte(line))
	}
	s := h.m.Lookup(h.sid)
	s.mu.Lock()
	bg := len(s.bgTasks)
	s.mu.Unlock()
	if bg != 1 {
		t.Fatalf("bg tasks %d", bg)
	}
	// Someone else writes to the transcript meanwhile; with a background
	// task running the warm process must be kept.
	writeTranscript(t, h.store, h.sid, h.cwd, `{"type":"user","uuid":"ext-1","message":{"role":"user","content":"typed in VS Code"}}`)
	for _, line := range []string{
		`{"type":"system","subtype":"task_notification","task_id":"b1","status":"completed"}`,
		`{"type":"system","subtype":"session_state_changed","state":"running"}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0},"parent_tool_use_id":null}`,
		`{"type":"result","subtype":"success","result":"done","num_turns":1,"session_id":"x"}`,
		`{"type":"system","subtype":"session_state_changed","state":"idle"}`,
	} {
		h.feed([]byte(line))
	}
	wantSig(t, signature(h.events()), "user_prompt busy result:ok idle busy text_start text_delta+ text_end result:ok idle")
	_ = l
}

func writeTranscript(t *testing.T, store *transcript.Store, id, cwd string, lines ...string) {
	t.Helper()
	dir := filepath.Join(store.ProjectsDir(), transcript.ProjectDirName(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func finishTurn(h *harness, text string) {
	for _, line := range []string{
		`{"type":"system","subtype":"session_state_changed","state":"running"}`,
		`{"type":"result","subtype":"success","result":"` + text + `","num_turns":1,"session_id":"x"}`,
		`{"type":"system","subtype":"session_state_changed","state":"idle"}`,
	} {
		h.feed([]byte(line))
	}
}

// External writes (VS Code / terminal) force a restart with --resume; our
// own writes don't.
func TestExternalWriteRestartsProcess(t *testing.T) {
	h := newHarness(t, ClassicDisplay())
	h.prompt("one")
	first := h.waitLaunch()
	// the CLI writes the transcript during our turn
	writeTranscript(t, h.store, h.sid, h.cwd, `{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"one"}}`, `{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"role":"assistant","content":[{"type":"text","text":"r"}]}}`)
	finishTurn(h, "r")

	h.prompt("two") // nothing external: reuse
	select {
	case <-h.procs:
		t.Fatal("restarted without external write")
	case <-time.After(50 * time.Millisecond):
	}
	writeTranscript(t, h.store, h.sid, h.cwd, `{"type":"user","uuid":"u2","parentUuid":"a1","message":{"role":"user","content":"two"}}`, `{"type":"assistant","uuid":"a2","parentUuid":"u2","message":{"role":"assistant","content":[{"type":"text","text":"r2"}]}}`)
	finishTurn(h, "r2")

	// external write
	writeTranscript(t, h.store, h.sid, h.cwd, `{"type":"user","uuid":"ext","parentUuid":"a2","message":{"role":"user","content":"from vscode"}}`)
	h.prompt("three")
	second := h.waitLaunch()
	if !first.proc.closed {
		t.Error("old process not closed")
	}
	if !hasArg(second.spec.Args, "--resume") || hasArg(second.spec.Args, "--session-id") {
		t.Errorf("relaunch args %v", second.spec.Args)
	}
}

func TestFailedTurnRecycles(t *testing.T) {
	h := newHarness(t, ClassicDisplay())
	h.prompt("one")
	h.waitLaunch()
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"running"}`))
	h.feed([]byte(`{"type":"result","subtype":"error_during_execution","errors":["boom"],"terminal_reason":"model_error","num_turns":1,"session_id":"x"}`))
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"idle"}`))
	if r := lastOf(h.events(), "result"); r["text"] != "boom" || r["success"] != false {
		t.Errorf("result %v", r)
	}
	h.prompt("two")
	h.waitLaunch() // recycled
}

func TestEccCommands(t *testing.T) {
	h := newHarness(t, DisplayFromConfigDefaults())
	h.prompt("one")
	l := h.waitLaunch()
	finishTurn(h, "r")
	before := len(l.proc.sentLines())
	for _, cmd := range []string{"/ecc mode plan", "/ecc model sonnet", "/ecc tools hidden", "/ecc status", "/ecc bogus"} {
		if _, err := h.m.Prompt(h.sid, cmd, nil); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	var notes []string
	for time.Now().Before(deadline) {
		notes = nil
		for _, e := range h.events() {
			if e["type"] == "notification" {
				notes = append(notes, e["message"].(string))
			}
		}
		if len(notes) == 5 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	joined := strings.Join(notes, " | ")
	for _, want := range []string{"Permission mode: plan", "Model: Sonnet 5.5", "Tool display: hidden", "warm pid 4242", "/ecc mode default|auto|plan|acceptEdits"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if h.m.Display().Tools != "hidden" {
		t.Error("tools not applied")
	}
	// nothing went to Claude as a prompt
	for _, s := range l.proc.sentLines()[before:] {
		if strings.Contains(s, `"type":"user"`) {
			t.Errorf("/ecc leaked to claude: %s", s)
		}
	}
	l.proc.mu.Lock()
	reqs := strings.Join(l.proc.reqs, ",")
	l.proc.mu.Unlock()
	if !strings.Contains(reqs, "set_permission_mode") || !strings.Contains(reqs, "set_model") {
		t.Errorf("control requests %s", reqs)
	}
	for _, e := range h.events() {
		if e["type"] == "user_prompt" && strings.HasPrefix(e["text"].(string), "/ecc") {
			t.Error("/ecc must not emit user_prompt")
		}
	}
}

func DisplayFromConfigDefaults() Display {
	return Display{Tools: "collapsed", Thinking: true, Rich: true, MaxOutput: 1536, MaxInput: 300, ShortOptions: true}
}

func TestInterruptWhilePermissionPending(t *testing.T) {
	h := newHarness(t, DisplayFromConfigDefaults())
	h.prompt("write it")
	l := h.waitLaunch()
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"running"}`))
	h.feed([]byte(`{"type":"control_request","request_id":"req-9","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"/x/a.txt","content":"hi"},"tool_use_id":"tu1","permission_suggestions":[{"type":"addRules","rules":[{"toolName":"Write"}],"behavior":"allow","destination":"projectSettings"}]}}`))
	if st := h.m.StatusOf(h.sid); st != "awaiting" {
		t.Fatalf("state %s", st)
	}
	pr := lastOf(h.events(), "permission_request")
	if got := string(jsonx.Marshal(pr["options"])); got != `[{"key":"allow","text":"Yes"},{"key":"allowAlways","text":"Always (project)"},{"key":"deny","text":"No"}]` {
		t.Errorf("options %s", got)
	}
	if pr["description"] != "Write a.txt (1 line)" {
		t.Errorf("desc %v", pr["description"])
	}
	h.m.Lookup(h.sid).Interrupt()
	if r := lastOf(h.events(), "permission_result"); r["decision"] != "denied" {
		t.Errorf("permission_result %v", r)
	}
	var deny bool
	for _, s := range l.proc.sentLines() {
		if strings.Contains(s, `"request_id":"req-9"`) && strings.Contains(s, `"behavior":"deny"`) {
			deny = true
		}
	}
	l.proc.mu.Lock()
	reqs := strings.Join(l.proc.reqs, ",")
	l.proc.mu.Unlock()
	if !deny || !strings.Contains(reqs, "interrupt") {
		t.Errorf("deny=%v reqs=%s", deny, reqs)
	}
	h.feed([]byte(`{"type":"result","subtype":"error_during_execution","terminal_reason":"aborted_tools","num_turns":1,"session_id":"x"}`))
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"idle"}`))
	if r := lastOf(h.events(), "result"); r["text"] != "Interrupted by user" {
		t.Errorf("result %v", r)
	}
	if h.m.StatusOf(h.sid) != "idle" || h.m.Lookup(h.sid).RespondPermission("allow") {
		t.Error("pending state not cleared")
	}
}

func waitState(t *testing.T, h *harness, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.m.StatusOf(h.sid) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("state %s, want %s", h.m.StatusOf(h.sid), want)
}

// Live log 2026-10-30 01:30–01:35 (CLI 2.1.312): Claude launched a
// background agent and replied; the CLI sent the result but no idle (it stays
// "running" while the agent works) and kept forwarding the subagent's frames
// (parent_tool_use_id set). The session must still go idle, and the next
// prompt must reach Claude at once instead of waiting in a queue for the
// agent to finish.
func TestBackgroundAgentResultWithoutIdle(t *testing.T) {
	h := newHarness(t, DisplayFromConfigDefaults())
	h.prompt("Let's do subagent progress")
	l := h.waitLaunch()
	main := func(ev string) {
		h.feed([]byte(`{"type":"stream_event","event":` + ev + `,"parent_tool_use_id":null}`))
	}
	sub := []string{
		`{"type":"assistant","message":{"model":"claude-opus-5-5","content":[{"type":"tool_use","id":"toolu_sub1","name":"Read","input":{"file_path":"/x/README.md"}}]},"parent_tool_use_id":"toolu_agent"}`,
		`{"type":"system","subtype":"task_progress","task_id":"ab59","tool_use_id":"toolu_agent","description":"Reading README.md","usage":{"total_tokens":1000,"tool_uses":1,"duration_ms":2000}}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_sub1","type":"tool_result","content":"# even-cc"}]},"parent_tool_use_id":"toolu_agent"}`,
	}
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"running"}`))
	main(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_agent","name":"Agent","input":{}}}`)
	h.feed([]byte(`{"type":"assistant","message":{"model":"claude-opus-5-5","content":[{"type":"tool_use","id":"toolu_agent","name":"Agent","input":{"description":"Add subagent progress and footer","subagent_type":"general-purpose","run_in_background":true,"prompt":"…"}}]},"parent_tool_use_id":null}`))
	main(`{"type":"content_block_stop","index":0}`)
	h.feed([]byte(`{"type":"system","subtype":"task_started","task_id":"ab59","tool_use_id":"toolu_agent","description":"Add subagent progress and footer","subagent_type":"general-purpose","is_backgrounded":true,"spawn_depth":1,"task_type":"local_agent"}`))
	h.feed([]byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_agent","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully."}]}]},"parent_tool_use_id":null}`))
	main(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	main(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Started."}}`)
	main(`{"type":"content_block_stop","index":0}`)
	h.feed([]byte(`{"type":"result","subtype":"success","result":"Started.","num_turns":2,"session_id":"x"}`))
	// no idle; the subagent keeps streaming
	for _, f := range sub {
		h.feed([]byte(f))
	}
	waitState(t, h, "idle")
	for _, f := range sub { // more subagent traffic while idle: no busy
		h.feed([]byte(f))
	}
	time.Sleep(80 * time.Millisecond)
	if st := h.m.StatusOf(h.sid); st != "idle" {
		t.Fatalf("subagent frames made the session %s", st)
	}

	// the user's next prompt goes to the CLI immediately
	h.prompt("A question for you")
	users := 0
	for _, s := range l.proc.sentLines() {
		if strings.Contains(s, `"type":"user"`) {
			users++
		}
	}
	if users != 2 {
		t.Fatalf("prompt not sent to claude (user lines %d)", users)
	}
	main(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	main(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Answer."}}`)
	main(`{"type":"content_block_stop","index":0}`)
	h.feed([]byte(`{"type":"result","subtype":"success","result":"Answer.","num_turns":1,"session_id":"x"}`))
	waitState(t, h, "idle") // still no CLI idle: the agent runs on

	// the agent finishes; Claude reports on its own, then the CLI goes idle
	h.feed([]byte(`{"type":"system","subtype":"task_notification","task_id":"ab59","tool_use_id":"toolu_agent","status":"completed","summary":"done","usage":{"total_tokens":2000,"tool_uses":40,"duration_ms":900000}}`))
	main(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	main(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The agent finished."}}`)
	main(`{"type":"content_block_stop","index":0}`)
	h.feed([]byte(`{"type":"result","subtype":"success","result":"The agent finished.","num_turns":1,"session_id":"x"}`))
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"idle"}`))
	waitState(t, h, "idle")
	wantSig(t, signature(h.events()), strings.Join([]string{
		"user_prompt busy tool_start:Agent tool_end:Agent text_start text_delta+ text_end result:ok idle",
		"user_prompt busy text_start text_delta+ text_end result:ok idle",
		"busy text_start text_delta+ text_end result:ok idle",
	}, " "))
}
