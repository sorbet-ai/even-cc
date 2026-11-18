package claude

import (
	"strings"
	"testing"
	"time"
)

// Talk while busy: synthetic CLI sequences shaped like 2.1.312 output for a
// main-agent Bash call.

const talkAfter = 60 * time.Millisecond

func talkDisplay() Display {
	d := DisplayFromConfigDefaults()
	d.TalkWhileBusy = true
	d.TalkWhileBusyAfter = talkAfter
	return d
}

type talkHarness struct{ *harness }

func newTalkHarness(t *testing.T, d Display) *talkHarness {
	h := &talkHarness{newHarness(t, d)}
	h.prompt("run the loop")
	h.waitLaunch()
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"running"}`))
	return h
}

func (h *talkHarness) se(ev string) {
	h.feed([]byte(`{"type":"stream_event","event":` + ev + `,"parent_tool_use_id":null}`))
}

// bash: the model streams a Bash tool_use; the CLI then executes it.
func (h *talkHarness) bash(id string) {
	h.se(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"` + id + `","name":"Bash","input":{}}}`)
	h.se(`{"type":"content_block_stop","index":0}`)
	h.feed([]byte(`{"type":"assistant","message":{"model":"claude-opus-5-5","content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":"for i in 1 2 3 4; do date; sleep 5; done","description":"Print the date 4 times"}}]},"parent_tool_use_id":null}`))
	h.se(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":40}}`)
	h.se(`{"type":"message_stop"}`)
}

func (h *talkHarness) bashResult(id string) {
	h.feed([]byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"` + id + `","type":"tool_result","content":"Thu Oct 30\nThu Oct 30","is_error":false}]},"parent_tool_use_id":null}`))
}

func (h *talkHarness) reply(text string) {
	h.se(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	h.se(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + text + `"}}`)
	h.se(`{"type":"content_block_stop","index":0}`)
	h.feed([]byte(`{"type":"result","subtype":"success","result":"` + text + `","num_turns":2,"session_id":"x"}`))
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"idle"}`))
}

func (h *talkHarness) sig() string { return strings.Join(signature(h.events()), " ") }

// waitSig waits until the event signature ends with suffix.
func (h *talkHarness) waitSig(suffix string) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.HasSuffix(h.sig(), suffix) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("events %q never ended with %q", h.sig(), suffix)
}

func (h *talkHarness) userLines() []string {
	var out []string
	for _, s := range h.cur.proc.sentLines() {
		if strings.Contains(s, `"type":"user"`) {
			out = append(out, s)
		}
	}
	return out
}

func TestTalkLongToolIdleThenBusyBeforeToolEnd(t *testing.T) {
	h := newTalkHarness(t, talkDisplay())
	h.bash("tb1")
	h.waitSig("tool_start:Bash idle")
	if st := h.m.StatusOf(h.sid); st != "idle" {
		t.Errorf("state while the app was told idle: %s", st)
	}
	time.Sleep(3 * talkAfter) // still running: exactly one idle
	h.bashResult("tb1")
	h.reply("Done.")
	wantSig(t, signature(h.events()), "user_prompt busy tool_start:Bash idle busy tool_end:Bash text_start text_delta+ text_end result:ok idle")
}

func TestTalkShortToolNoFlip(t *testing.T) {
	h := newTalkHarness(t, talkDisplay())
	h.bash("tb1")
	time.Sleep(talkAfter / 3)
	h.bashResult("tb1")
	time.Sleep(2 * talkAfter)
	h.reply("Done.")
	wantSig(t, signature(h.events()), "user_prompt busy tool_start:Bash tool_end:Bash text_start text_delta+ text_end result:ok idle")
}

// Each long tool gets its own pair; never during streamed text.
func TestTalkTwoLongToolsOnePairEach(t *testing.T) {
	d := talkDisplay()
	d.Tools = "full" // collapsed would merge the two Bash calls into one line
	h := newTalkHarness(t, d)
	h.bash("tb1")
	h.waitSig("idle")
	h.bashResult("tb1")
	h.bash("tb2")
	h.waitSig("tool_start:Bash idle")
	h.bashResult("tb2")
	h.reply("Done.")
	wantSig(t, signature(h.events()), "user_prompt busy tool_start:Bash idle busy tool_end:Bash tool_start:Bash idle busy tool_end:Bash text_start text_delta+ text_end result:ok idle")
}

func TestTalkNoIdleWhilePermissionPending(t *testing.T) {
	h := newTalkHarness(t, talkDisplay())
	h.bash("tb1")
	h.feed([]byte(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"for i in 1 2 3 4; do date; sleep 5; done"},"tool_use_id":"tb1"}}`))
	time.Sleep(3 * talkAfter)
	if s := h.sig(); strings.Contains(s, "permission_request idle") || h.m.StatusOf(h.sid) != "awaiting" {
		t.Fatalf("idle while permission pending: %s (state %s)", s, h.m.StatusOf(h.sid))
	}
	// Allowed: now the tool runs, and after the threshold the user may talk.
	if !h.m.Lookup(h.sid).RespondPermission("allow") {
		t.Fatal("permission not answered")
	}
	h.waitSig("permission_result:allowed idle")
	h.bashResult("tb1")
	h.reply("Done.")
	wantSig(t, signature(h.events()), "user_prompt busy tool_start:Bash permission_request permission_result:allowed idle busy tool_end:Bash text_start text_delta+ text_end result:ok idle")
}

// A long tool that hits a permission prompt after the app went idle (here a
// subagent's): busy comes back before the prompt.
func TestTalkPermissionAfterIdleSendsBusyFirst(t *testing.T) {
	h := newTalkHarness(t, talkDisplay())
	h.bash("tb1")
	h.waitSig("idle")
	h.feed([]byte(`{"type":"control_request","request_id":"req-2","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"/x/a","content":"y"},"tool_use_id":"tw9"}}`))
	if !strings.HasSuffix(h.sig(), "idle busy permission_request") {
		t.Fatalf("events %s", h.sig())
	}
}

// An auto-allowed request (a subagent's ls) shows nothing: no flip.
func TestTalkAutoAllowedRequestNoFlip(t *testing.T) {
	h := newTalkHarness(t, talkDisplay())
	h.bash("tb1")
	h.waitSig("idle")
	h.feed([]byte(`{"type":"control_request","request_id":"req-3","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls -la"},"tool_use_id":"tl1"}}`))
	if !strings.HasSuffix(h.sig(), "tool_start:Bash idle") || h.m.StatusOf(h.sid) != "idle" {
		t.Fatalf("events %s state %s", h.sig(), h.m.StatusOf(h.sid))
	}
}

func TestTalkMidTurnPromptForwarded(t *testing.T) {
	h := newTalkHarness(t, talkDisplay())
	h.bash("tb1")
	h.waitSig("idle")
	h.prompt("also tell me the current year")
	users := h.userLines()
	if len(users) != 2 || !strings.Contains(users[1], `"content":"also tell me the current year"`) {
		t.Fatalf("mid-turn prompt not written to the CLI: %v", users)
	}
	time.Sleep(3 * talkAfter) // same tool: no second idle
	h.bashResult("tb1")
	h.reply("Done. It is 2026.")
	wantSig(t, signature(h.events()), "user_prompt busy tool_start:Bash idle user_prompt busy tool_end:Bash text_start text_delta+ text_end result:ok idle")
}

func TestTalkStopWordInterrupts(t *testing.T) {
	for _, word := range []string{"Stop.", "stop it!", "  CANCEL ", "Halt", "/ecc stop"} {
		h := newTalkHarness(t, talkDisplay())
		h.bash("tb1")
		h.waitSig("idle")
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
		want := "user_prompt busy tool_start:Bash idle user_prompt busy tool_end:Bash result:fail idle"
		if word == "/ecc stop" { // a server command: notification, no user_prompt
			want = "user_prompt busy tool_start:Bash idle busy notification tool_end:Bash result:fail idle"
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

func TestTalkOffAndEccStatus(t *testing.T) {
	h := newTalkHarness(t, talkDisplay())
	if _, err := h.m.Prompt(h.sid, "/ecc busy off", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Prompt(h.sid, "/ecc status", nil); err != nil {
		t.Fatal(err)
	}
	h.bash("tb1")
	time.Sleep(3 * talkAfter)
	h.bashResult("tb1")
	h.reply("Done.")
	wantSig(t, signature(h.events()), "user_prompt busy notification notification tool_start:Bash tool_end:Bash text_start text_delta+ text_end result:ok idle")
	var notes []string
	for _, e := range h.events() {
		if e["type"] == "notification" {
			notes = append(notes, e["message"].(string))
		}
	}
	if notes[0] != "Talk while busy: off" || !strings.Contains(notes[1], "talk while busy off") {
		t.Errorf("notifications %q", notes)
	}
	d := h.m.Display()
	d.TalkWhileBusy = true
	if got := talkText(d); got != "talk while busy on (0.06s)" {
		t.Errorf("talkText %q", got)
	}
}

// A foreground subagent is a long main-agent tool: its progress updates are
// held while the app is told idle and sent right after busy.
func TestTalkForegroundAgentUpdatesHeld(t *testing.T) {
	d := agentDisplay()
	d.TalkWhileBusy = true
	d.TalkWhileBusyAfter = talkAfter
	h := newAgentHarness(t, d)
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"running"}`)
	h.agentCall("ta1", "Count files", false)
	h.lines(taskStarted("t1", "ta1", "Count files", false))
	th := &talkHarness{h.harness}
	th.waitSig("tool_end:Agent idle")
	h.advance(20 * time.Second)
	h.lines(taskProgress("t1", "ta1", "Running ls", 3, 20000, "Listing files"))
	h.tick()
	if s := th.sig(); !strings.HasSuffix(s, "tool_end:Agent idle") {
		t.Fatalf("update sent while the app was told idle: %s", s)
	}
	h.lines(taskNotification("t1", "ta1", "completed", 4, 25000, "3 files"))
	h.lines(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"ta1","type":"tool_result","content":"3 files"}]},"parent_tool_use_id":null}`)
	got := agentSummaries(h.events(), "ta1")
	wantStrings(t, got, []string{"Agent: Count files", "Agent: Count files · done · 4 tools · 25s"})
	if s := th.sig(); !strings.HasSuffix(s, "tool_end:Agent idle busy tool_end:Agent") {
		t.Errorf("events %s", s)
	}
}
