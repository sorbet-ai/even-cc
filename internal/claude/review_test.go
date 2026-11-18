package claude

import (
	"strings"
	"testing"
	"time"
)

// Regression tests for the 2026-10-30 engineering review.

const (
	stateRunning = `{"type":"system","subtype":"session_state_changed","state":"running"}`
	bgAgent      = `{"type":"system","subtype":"task_started","task_id":"t1","tool_use_id":"ag1","description":"x","is_backgrounded":true,"task_type":"local_agent"}`
	resultOK     = `{"type":"result","subtype":"success","result":"launched","num_turns":1,"duration_ms":10,"session_id":"x"}`
)

func subagentPerm(reqID, agentID string) []byte {
	agent := ""
	if agentID != "" {
		agent = `,"agent_id":"` + agentID + `"`
	}
	return []byte(`{"type":"control_request","request_id":"` + reqID + `","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf x"},"tool_use_id":"tb9"` + agent + `}}`)
}

func (h *harness) userLineCount() int {
	n := 0
	for _, s := range h.cur.proc.sentLines() {
		if strings.Contains(s, `"type":"user"`) {
			n++
		}
	}
	return n
}

func (h *harness) responsesFor(reqID string) []string {
	var out []string
	for _, s := range h.cur.proc.sentLines() {
		if strings.Contains(s, `"control_response"`) && strings.Contains(s, `"request_id":"`+reqID+`"`) {
			out = append(out, s)
		}
	}
	return out
}

// A background subagent's permission request outlives the main turn's
// result: it stays answerable, and was never answered silently.
func TestSubagentPermissionSurvivesResult(t *testing.T) {
	for _, agentID := range []string{"a1", ""} {
		t.Run("agent_id="+agentID, func(t *testing.T) {
			h := newHarness(t, DisplayFromConfigDefaults())
			h.prompt("go")
			h.waitLaunch()
			h.feed([]byte(stateRunning))
			h.feed([]byte(bgAgent))
			h.feed(subagentPerm("req-bg", agentID))
			if agentID != "" {
				// mid-turn: a prompt goes to the CLI, not into the subagent's request
				h.prompt("also this")
				if n := h.userLineCount(); n != 2 || len(h.responsesFor("req-bg")) != 0 {
					t.Fatalf("user lines %d, responses %v", n, h.responsesFor("req-bg"))
				}
			}
			h.feed([]byte(resultOK))
			s := h.m.Lookup(h.sid)
			if st := s.State(); st != "awaiting" {
				t.Errorf("state after result %s", st)
			}
			if r := h.responsesFor("req-bg"); len(r) != 0 {
				t.Fatalf("request answered at turn end: %v", r)
			}
			// the next prompt starts a turn instead of denying the subagent's request
			before := h.userLineCount()
			h.prompt("next thing")
			if h.userLineCount() != before+1 || len(h.responsesFor("req-bg")) != 0 {
				t.Fatalf("prompt consumed by the subagent's request")
			}
			if !s.RespondPermission("allow") {
				t.Fatal("subagent permission no longer answerable")
			}
			if r := h.responsesFor("req-bg"); len(r) != 1 || !strings.Contains(r[0], `"behavior":"allow"`) {
				t.Errorf("responses %v", r)
			}
		})
	}
}

// Fields of an unexpected type cost that field, not the line.
func TestLenientCLIDecoding(t *testing.T) {
	h := newHarness(t, DisplayFromConfigDefaults())
	h.prompt("go")
	h.waitLaunch()
	h.feed([]byte(`{"type":"control_request","request_id":"r1","attempt":"1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"make"},"tool_use_id":"tb1"}}`))
	if st := h.m.StatusOf(h.sid); st != "awaiting" {
		t.Fatalf("permission request lost: state %s", st)
	}
	h.m.Lookup(h.sid).RespondPermission("deny")
	h.feed([]byte(`{"type":"result","subtype":"error_during_execution","errors":[{"message":"overloaded"},"plain"],"num_turns":1,"duration_ms":5,"session_id":"x"}`))
	if st := h.m.StatusOf(h.sid); st != "idle" {
		t.Fatalf("result lost: state %s", st)
	}
	if r := lastOf(h.events(), "result"); r["text"] != "overloaded\nplain" {
		t.Errorf("result text %q", r["text"])
	}
}

type gatedClose struct {
	*fakeProc
	gate chan struct{}
}

func (g *gatedClose) Close() { <-g.gate; g.fakeProc.Close() }

// recycleHarness: the first process fails a turn, so the next prompt
// replaces it, and closing it blocks until gate is closed.
func recycleHarness(t *testing.T) (*harness, chan struct{}) {
	h := newHarness(t, DisplayFromConfigDefaults())
	gate := make(chan struct{})
	orig := h.m.launch
	first := true
	h.m.launch = func(spec ProcSpec, onLine func([]byte), onExit func(procHandle)) (procHandle, error) {
		p, err := orig(spec, onLine, onExit)
		if first {
			first = false
			return &gatedClose{p.(*fakeProc), gate}, err
		}
		return p, err
	}
	h.prompt("go")
	h.waitLaunch()
	h.feed([]byte(`{"type":"result","subtype":"error_during_execution","num_turns":1,"duration_ms":10,"session_id":"x","errors":["boom"]}`))
	h.prompt("second") // recycle: closing the old process blocks on gate
	return h, gate
}

func (h *harness) noLaunch(within time.Duration) {
	h.t.Helper()
	select {
	case <-h.procs:
		h.t.Fatal("a claude process was started")
	case <-time.After(within):
	}
}

func TestInterruptDuringRecycle(t *testing.T) {
	h, gate := recycleHarness(t)
	s := h.m.Lookup(h.sid)
	s.Interrupt()
	close(gate)
	h.noLaunch(200 * time.Millisecond)
	if st := s.State(); st != "idle" {
		t.Errorf("state %s", st)
	}
	evs := h.events()
	if r := lastOf(evs, "result"); r["text"] != "Interrupted by user" {
		t.Errorf("last result %v", r)
	}
	if sig := signature(evs); sig[len(sig)-1] != "idle" {
		t.Errorf("events %v", sig)
	}
	// the session still works afterwards
	h.prompt("third")
	h.waitLaunch()
}

func TestShutdownDuringRecycle(t *testing.T) {
	h, gate := recycleHarness(t)
	done := make(chan struct{})
	go func() { h.m.Shutdown(); close(done) }()
	time.Sleep(20 * time.Millisecond)
	close(gate)
	<-done
	h.noLaunch(200 * time.Millisecond)
	if _, err := h.m.Prompt(h.sid, "after shutdown", nil); err != nil {
		t.Fatal(err)
	}
	h.noLaunch(200 * time.Millisecond)
}

func TestModelNameValidated(t *testing.T) {
	for _, ok := range []string{"opus", "claude-opus-5-5", "claude-opus-5-5[1m]", "us.anthropic.claude-sonnet-5-5-v1:0", "arn:aws:bedrock:us-east-1:123:inference-profile/x", "default"} {
		if !validModel(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{`a"&calc`, "a b", "--dangerously-skip-permissions", "", "x;rm", "a|b", "%PATH%"} {
		if validModel(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	h := newHarness(t, DisplayFromConfigDefaults())
	h.prompt("go")
	l := h.waitLaunch()
	s := h.m.Lookup(h.sid)
	if reply := s.setModel(`a"&calc`); !strings.HasPrefix(reply, "Invalid model name") {
		t.Errorf("reply %q", reply)
	}
	s.mu.Lock()
	s.model = `a"&calc` // as if it came from somewhere else
	spec := h.m.procSpec(s)
	s.mu.Unlock()
	if hasArg(spec.Args, "--model") {
		t.Errorf("invalid model passed: %v", spec.Args)
	}
	_ = l
}

// No configured mode: claude gets no --permission-mode and uses the user's
// permissions.defaultMode; a configured or /ecc-chosen mode is passed.
func TestPermissionModeFromUserSettings(t *testing.T) {
	h := newHarness(t, DisplayFromConfigDefaults())
	h.prompt("go")
	h.waitLaunch()
	s := h.m.Lookup(h.sid)
	s.mu.Lock()
	spec := h.m.procSpec(s)
	s.mu.Unlock()
	if hasArg(spec.Args, "--permission-mode") {
		t.Errorf("mode passed without one configured: %v", spec.Args)
	}
	s.mu.Lock()
	s.permMode = "auto"
	spec = h.m.procSpec(s)
	s.mu.Unlock()
	if i := indexOf(spec.Args, "--permission-mode"); i < 0 || spec.Args[i+1] != "auto" {
		t.Errorf("chosen mode not passed: %v", spec.Args)
	}
}

func indexOf(args []string, a string) int {
	for i, x := range args {
		if x == a {
			return i
		}
	}
	return -1
}
