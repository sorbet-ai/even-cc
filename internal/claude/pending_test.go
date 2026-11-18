package claude

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"even-cc/internal/guard"
)

// Prompts, answers, interrupts and process exits around a pending
// AskUserQuestion / permission request, including the 2026-10-30 incident:
// a long Bash with talk-while-busy, backgrounded by the CLI, then a question
// the glasses never showed, then a prompt sent instead of an answer.

const askReq = `{"type":"control_request","request_id":"req-q","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Which one?","header":"Pick","options":[{"label":"A","description":"a"},{"label":"B","description":"b"}],"multiSelect":false}]},"tool_use_id":"tq1"}}`

func (h *talkHarness) incidentUpToQuestion() {
	h.bash("tb1")
	h.waitSig("tool_start:Bash idle")
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
	h := newTalkHarness(t, talkDisplay())
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
	wantSig(t, signature(h.events()), "user_prompt busy tool_start:Bash idle busy tool_end:Bash text_start text_delta+ text_end tool_start:AskUserQuestion user_question user_prompt question_answer")
	// JSON-looking text stays text
	h2 := newTalkHarness(t, talkDisplay())
	h2.incidentUpToQuestion()
	h2.prompt("42")
	if a := h2.controlResponses()[0]["updatedInput"].(map[string]any)["answers"]; a.(map[string]any)["Which one?"] != "42" {
		t.Errorf("answers %v", a)
	}
}

func TestPromptDeniesPendingPermission(t *testing.T) {
	h := newTalkHarness(t, talkDisplay())
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
	h := newTalkHarness(t, talkDisplay())
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
	end := func(h *talkHarness) {
		h.feed([]byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"tq1","type":"tool_result","content":"answered"}]},"parent_tool_use_id":null}`))
		h.reply("Ok.")
		if st := h.m.StatusOf(h.sid); st != "idle" {
			t.Errorf("state %s", st)
		}
	}
	t.Run("skip", func(t *testing.T) {
		h := newTalkHarness(t, talkDisplay())
		h.incidentUpToQuestion()
		h.m.Lookup(h.sid).RespondQuestion("skip")
		h.m.Lookup(h.sid).RespondQuestion("late duplicate") // nothing pending: dropped
		end(h)
	})
	t.Run("empty", func(t *testing.T) {
		h := newTalkHarness(t, talkDisplay())
		h.incidentUpToQuestion()
		h.m.Lookup(h.sid).RespondQuestion("")
		end(h)
	})
	t.Run("interrupt", func(t *testing.T) {
		h := newTalkHarness(t, talkDisplay())
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
		h := newTalkHarness(t, talkDisplay())
		h.incidentUpToQuestion()
		h.feed([]byte(`{"type":"control_cancel_request","request_id":"req-q"}`))
		h.m.Lookup(h.sid).RespondQuestion("A")
		if len(h.controlResponses()) != 0 {
			t.Errorf("answered a cancelled request")
		}
		end(h)
	})
	t.Run("process exits", func(t *testing.T) {
		h := newTalkHarness(t, talkDisplay())
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
// interrupts and SSE clients on one session.
func TestPendingConcurrentNoRace(t *testing.T) {
	h := newTalkHarness(t, talkDisplay())
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
	h := newTalkHarness(t, talkDisplay())
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
