package claude

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"even-cc/internal/guard"
)

// Prompts mid-turn, and prompts, answers, interrupts and process exits
// around a pending AskUserQuestion / permission request, including the
// 2026-10-30 incident: a long Bash, backgrounded by the CLI, then a question
// the glasses never showed, then a prompt sent instead of an answer. The CLI
// sequences are shaped like 2.1.312 output for a main-agent Bash call.

type turnHarness struct{ *harness }

func newTurnHarness(t *testing.T) *turnHarness {
	h := &turnHarness{newHarness(t, DisplayFromConfigDefaults())}
	h.prompt("run the loop")
	h.waitLaunch()
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"running"}`))
	return h
}

func (h *turnHarness) se(ev string) {
	h.feed([]byte(`{"type":"stream_event","event":` + ev + `,"parent_tool_use_id":null}`))
}

// bash: the model streams a Bash tool_use; the CLI then executes it.
func (h *turnHarness) bash(id string) {
	h.se(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"` + id + `","name":"Bash","input":{}}}`)
	h.se(`{"type":"content_block_stop","index":0}`)
	h.feed([]byte(`{"type":"assistant","message":{"model":"claude-opus-5-5","content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":"for i in 1 2 3 4; do date; sleep 5; done","description":"Print the date 4 times"}}]},"parent_tool_use_id":null}`))
	h.se(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":40}}`)
	h.se(`{"type":"message_stop"}`)
}

func (h *turnHarness) bashResult(id string) {
	h.feed([]byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"` + id + `","type":"tool_result","content":"Thu Oct 30\nThu Oct 30","is_error":false}]},"parent_tool_use_id":null}`))
}

func (h *turnHarness) reply(text string) {
	h.se(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	h.se(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + text + `"}}`)
	h.se(`{"type":"content_block_stop","index":0}`)
	h.feed([]byte(`{"type":"result","subtype":"success","result":"` + text + `","num_turns":2,"session_id":"x"}`))
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"idle"}`))
}

func (h *turnHarness) sig() string { return strings.Join(signature(h.events()), " ") }

func (h *turnHarness) userLines() []string {
	var out []string
	for _, s := range h.cur.proc.sentLines() {
		if strings.Contains(s, `"type":"user"`) {
			out = append(out, s)
		}
	}
	return out
}

// A prompt during a long tool goes to the CLI, which folds it into the
// turn; the session stays busy throughout.
func TestMidTurnPromptForwarded(t *testing.T) {
	h := newTurnHarness(t)
	h.bash("tb1")
	time.Sleep(50 * time.Millisecond)
	if st := h.m.StatusOf(h.sid); st != "busy" {
		t.Errorf("state during the tool %s", st)
	}
	h.prompt("also tell me the current year")
	users := h.userLines()
	if len(users) != 2 || !strings.Contains(users[1], `"content":"also tell me the current year"`) {
		t.Fatalf("mid-turn prompt not written to the CLI: %v", users)
	}
	h.bashResult("tb1")
	h.reply("Done. It is 2026.")
	wantSig(t, signature(h.events()), "user_prompt busy tool_start:Bash user_prompt tool_end:Bash text_start text_delta+ text_end result:ok idle")
}

func TestStopWordInterrupts(t *testing.T) {
	for _, word := range []string{"Stop.", "stop it!", "  CANCEL ", "Halt", "/ecc stop"} {
		h := newTurnHarness(t)
		h.bash("tb1")
		h.prompt(word)
		h.cur.proc.mu.Lock()
		reqs := strings.Join(h.cur.proc.reqs, ",")
		h.cur.proc.mu.Unlock()
		if !strings.Contains(reqs, "interrupt") {
			t.Errorf("%q: no interrupt (requests %s)", word, reqs)
		}
		if n := len(h.userLines()); n != 1 {
			t.Errorf("%q forwarded to claude (%d user lines)", word, n)
		}
		h.bashResult("tb1")
		h.feed([]byte(`{"type":"result","subtype":"error_during_execution","terminal_reason":"aborted_tools","num_turns":1,"session_id":"x"}`))
		h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"idle"}`))
		want := "user_prompt busy tool_start:Bash user_prompt tool_end:Bash result:fail idle"
		if word == "/ecc stop" { // a server command: notification, no user_prompt
			want = "user_prompt busy tool_start:Bash notification tool_end:Bash result:fail idle"
		}
		wantSig(t, signature(h.events()), want)
	}
}

func TestIsStopWord(t *testing.T) {
	for text, want := range map[string]bool{
		"stop": true, "Stop.": true, "STOP IT!!": true, "stop  it": true, "cancel…": true, "halt?": true,
		"stop the tests": false, "don't stop": false, "": false, "stopped": false,
	} {
		if got := isStopWord(text); got != want {
			t.Errorf("%q: %v want %v", text, got, want)
		}
	}
}

const askReq = `{"type":"control_request","request_id":"req-q","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Which one?","header":"Pick","options":[{"label":"A","description":"a"},{"label":"B","description":"b"}],"multiSelect":false}]},"tool_use_id":"tq1"}}`

func (h *turnHarness) incidentUpToQuestion() {
	h.bash("tb1")
	h.feed([]byte(`{"type":"system","subtype":"task_started","task_id":"bi841l8z0","tool_use_id":"tb1","description":"Print the date","task_type":"local_bash"}`))
	h.feed([]byte(`{"type":"system","subtype":"task_notification","task_id":"bi841l8z0","tool_use_id":"tb1","status":"completed","summary":"done"}`))
	h.bashResult("tb1")
	h.se(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	h.se(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Before I go on:"}}`)
	h.se(`{"type":"content_block_stop","index":0}`)
	h.se(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tq1","name":"AskUserQuestion","input":{}}}`)
	h.se(`{"type":"content_block_stop","index":1}`)
	h.feed([]byte(`{"type":"assistant","message":{"model":"claude-opus-5-5","content":[{"type":"tool_use","id":"tq1","name":"AskUserQuestion","input":{"questions":[]}}]},"parent_tool_use_id":null}`))
	h.feed([]byte(askReq))
	if st := h.m.StatusOf(h.sid); st != "awaiting" {
		h.t.Fatalf("state %s", st)
	}
}

func (h *harness) controlResponses() []map[string]any {
	var out []map[string]any
	for _, s := range h.cur.proc.sentLines() {
		if strings.Contains(s, `"control_response"`) {
			var m map[string]any
			_ = json.Unmarshal([]byte(s), &m)
			out = append(out, m["response"].(map[string]any)["response"].(map[string]any))
		}
	}
	return out
}

func TestPromptAnswersPendingQuestion(t *testing.T) {
	h := newTurnHarness(t)
	h.incidentUpToQuestion()
	h.prompt("Are you seeing any of this?")
	if n := len(h.userLines()); n != 1 {
		t.Fatalf("prompt written to the blocked CLI as a user message (%d user lines)", n)
	}
	rs := h.controlResponses()
	if len(rs) != 1 || rs[0]["behavior"] != "allow" {
		t.Fatalf("responses %v", rs)
	}
	ans := rs[0]["updatedInput"].(map[string]any)["answers"].(map[string]any)
	if ans["Which one?"] != "Are you seeing any of this?" {
		t.Errorf("answers %v", ans)
	}
	if st := h.m.StatusOf(h.sid); st != "busy" {
		t.Errorf("state after the answer %s", st)
	}
	wantSig(t, signature(h.events()), "user_prompt busy tool_start:Bash tool_end:Bash text_start text_delta+ text_end tool_start:AskUserQuestion user_question user_prompt question_answer")
	// JSON-looking text stays text
	h2 := newTurnHarness(t)
	h2.incidentUpToQuestion()
	h2.prompt("42")
	if a := h2.controlResponses()[0]["updatedInput"].(map[string]any)["answers"]; a.(map[string]any)["Which one?"] != "42" {
		t.Errorf("answers %v", a)
	}
}

// Faceclaw always offers allowAlways. With no suggestions to save it
// allows, and the tool is allowed for the rest of the session.
func TestAllowAlwaysWithoutSuggestions(t *testing.T) {
	h := newTurnHarness(t)
	s := h.m.Lookup(h.sid)
	canUse := func(reqID, tool, input string) {
		h.feed([]byte(`{"type":"control_request","request_id":"` + reqID + `","request":{"subtype":"can_use_tool","tool_name":"` + tool + `","input":` + input + `,"tool_use_id":"t-` + reqID + `"}}`))
	}
	canUse("req-1", "Bash", `{"command":"rm -rf build"}`)
	if s.RespondPermission("bogus") {
		t.Error("unoffered decision accepted")
	}
	if !s.RespondPermission("allowAlways") {
		t.Fatal("allowAlways refused")
	}
	r := h.responsesFor("req-1")
	if len(r) != 1 || !strings.Contains(r[0], `"behavior":"allow"`) || strings.Contains(r[0], "updatedPermissions") {
		t.Fatalf("response %v", r)
	}
	canUse("req-2", "Bash", `{"command":"make clean"}`)
	if r := h.responsesFor("req-2"); len(r) != 1 || !strings.Contains(r[0], `"behavior":"allow"`) {
		t.Errorf("same tool not allowed: %v", r)
	}
	canUse("req-3", "Write", `{"file_path":"x","content":"y"}`)
	if r := h.responsesFor("req-3"); len(r) != 0 {
		t.Errorf("other tool answered: %v", r)
	}
	if !strings.HasSuffix(h.sig(), "permission_request permission_result:always permission_request") {
		t.Errorf("events %s", h.sig())
	}
}

func TestPromptDeniesPendingPermission(t *testing.T) {
	h := newTurnHarness(t)
	h.bash("tb1")
	h.feed([]byte(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf build"},"tool_use_id":"tb1"}}`))
	h.prompt("no, keep the build dir")
	rs := h.controlResponses()
	if len(rs) != 1 || rs[0]["behavior"] != "deny" || !strings.Contains(rs[0]["message"].(string), "no, keep the build dir") {
		t.Fatalf("responses %v", rs)
	}
	if n := len(h.userLines()); n != 1 {
		t.Errorf("%d user lines", n)
	}
	if !strings.HasSuffix(h.sig(), "permission_request user_prompt permission_result:denied") {
		t.Errorf("events %s", h.sig())
	}
	// the next prompt (nothing pending) goes to the CLI as usual
	h.prompt("then just list it")
	if n := len(h.userLines()); n != 2 {
		t.Errorf("%d user lines", n)
	}
}

func TestStopWordWithPendingQuestionInterrupts(t *testing.T) {
	h := newTurnHarness(t)
	h.incidentUpToQuestion()
	h.prompt("stop")
	h.cur.proc.mu.Lock()
	reqs := strings.Join(h.cur.proc.reqs, ",")
	h.cur.proc.mu.Unlock()
	if !strings.Contains(reqs, "interrupt") {
		t.Errorf("no interrupt: %s", reqs)
	}
	if rs := h.controlResponses(); len(rs) != 1 || !strings.Contains(string(mustJSON(rs[0])), `"skip"`) {
		t.Errorf("question not skipped: %v", rs)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// The app's reactions to a question, each ending in a clean turn.
func TestQuestionAppReactions(t *testing.T) {
	end := func(h *turnHarness) {
		h.feed([]byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"tq1","type":"tool_result","content":"answered"}]},"parent_tool_use_id":null}`))
		h.reply("Ok.")
		if st := h.m.StatusOf(h.sid); st != "idle" {
			t.Errorf("state %s", st)
		}
	}
	t.Run("skip", func(t *testing.T) {
		h := newTurnHarness(t)
		h.incidentUpToQuestion()
		h.m.Lookup(h.sid).RespondQuestion("skip")
		h.m.Lookup(h.sid).RespondQuestion("late duplicate") // nothing pending: dropped
		end(h)
	})
	t.Run("empty", func(t *testing.T) {
		h := newTurnHarness(t)
		h.incidentUpToQuestion()
		h.m.Lookup(h.sid).RespondQuestion("")
		end(h)
	})
	t.Run("interrupt", func(t *testing.T) {
		h := newTurnHarness(t)
		h.incidentUpToQuestion()
		h.m.Lookup(h.sid).Interrupt()
		h.m.Lookup(h.sid).Interrupt() // twice
		h.m.Lookup(h.sid).RespondQuestion("A")
		h.feed([]byte(`{"type":"result","subtype":"error_during_execution","terminal_reason":"aborted_tools","num_turns":1,"session_id":"x"}`))
		h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"idle"}`))
		if !strings.HasSuffix(h.sig(), "question_answer result:fail idle") {
			t.Errorf("events %s", h.sig())
		}
	})
	t.Run("cli cancels", func(t *testing.T) {
		h := newTurnHarness(t)
		h.incidentUpToQuestion()
		h.feed([]byte(`{"type":"control_cancel_request","request_id":"req-q"}`))
		h.m.Lookup(h.sid).RespondQuestion("A")
		if len(h.controlResponses()) != 0 {
			t.Errorf("answered a cancelled request")
		}
		end(h)
	})
	t.Run("process exits", func(t *testing.T) {
		h := newTurnHarness(t)
		h.incidentUpToQuestion()
		h.cur.proc.mu.Lock()
		h.cur.proc.code = 143
		h.cur.proc.mu.Unlock()
		h.cur.onExit(h.cur.proc)
		h.cur.onExit(h.cur.proc) // twice
		s := h.m.Lookup(h.sid)
		s.RespondQuestion("A")
		s.Interrupt()
		if !s.RespondPermission("allow") == false {
			t.Error("permission answered with nothing pending")
		}
		h.feed([]byte(`{"type":"result","subtype":"success","result":"x","num_turns":1,"session_id":"x"}`)) // late output of the dead process
		if st := h.m.StatusOf(h.sid); st != "idle" {
			t.Errorf("state %s", st)
		}
		if !strings.HasSuffix(h.sig(), "user_question question_answer idle") {
			t.Errorf("events %s", h.sig())
		}
		// the next prompt starts a new process
		h.prompt("again")
		h.waitLaunch()
	})
}

// Everything at once, for the race detector: CLI output, prompts, answers,
// interrupts and WebSocket clients on one session.
func TestPendingConcurrentNoRace(t *testing.T) {
	h := newTurnHarness(t)
	h.incidentUpToQuestion()
	s := h.m.Lookup(h.sid)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	run := func(f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				f(i)
			}
		}()
	}
	run(func(i int) { h.feed([]byte(askReq)) })
	run(func(i int) { s.RespondQuestion("A") })
	run(func(i int) {
		if i%50 == 0 {
			s.Interrupt()
		}
	})
	run(func(i int) {
		if _, err := h.m.Prompt(h.sid, "hello", nil); err != nil {
			t.Error(err)
		}
	})
	run(func(i int) {
		c := h.hub.Subscribe(h.sid, i%2 == 0)
		h.hub.Unsubscribe(h.sid, c)
		_ = s.State()
	})
	run(func(i int) {
		h.feed([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}},"parent_tool_use_id":null}`))
	})
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// A panic while handling a CLI line is recovered: the session's lock is
// released and later lines and prompts still work.
func TestCLILinePanicRecovered(t *testing.T) {
	h := newTurnHarness(t)
	s := h.m.Lookup(h.sid)
	s.mu.Lock()
	saved := s.mapper
	s.mapper = nil // the next stream_event dereferences it
	s.mu.Unlock()
	before := guard.Panics.Load()
	h.se(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	if guard.Panics.Load() != before+1 {
		t.Fatal("no panic recovered")
	}
	done := make(chan string)
	go func() { done <- s.State() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("session lock still held after the panic")
	}
	s.mu.Lock()
	s.mapper = saved
	s.mu.Unlock()
	h.reply("Done.")
	if st := h.m.StatusOf(h.sid); st != "idle" {
		t.Errorf("state %s", st)
	}
}
