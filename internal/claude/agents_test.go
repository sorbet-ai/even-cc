package claude

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Synthetic CLI frames modelled on a recorded 2.1.312 run (foreground and
// background Agent calls with agentProgressSummaries on).

func agentDisplay() Display {
	d := DisplayFromConfigDefaults()
	d.SubagentProgress = true
	d.ProgressInterval = 15 * time.Second
	d.Footer = true
	return d
}

type agentHarness struct {
	*harness
	clock time.Time
}

func newAgentHarness(t *testing.T, d Display) *agentHarness {
	h := &agentHarness{harness: newHarness(t, d), clock: time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)}
	h.prompt("go")
	h.waitLaunch()
	h.m.Lookup(h.sid).mapper.now = func() time.Time { return h.clock }
	return h
}

func (h *agentHarness) advance(d time.Duration) { h.clock = h.clock.Add(d) }

func (h *agentHarness) se(ev string) {
	h.feed([]byte(`{"type":"stream_event","event":` + ev + `,"parent_tool_use_id":null}`))
}

func (h *agentHarness) lines(ls ...string) {
	for _, l := range ls {
		h.feed([]byte(l))
	}
}

func (h *agentHarness) tick() {
	s := h.m.Lookup(h.sid)
	s.mu.Lock()
	s.mapper.TickAgents()
	s.mu.Unlock()
}

// mainMessage: one main-agent API call with the given usage.
func (h *agentHarness) usage(in, cacheRead, cacheCreate, out, thinking int) {
	h.se(fmt.Sprintf(`{"type":"message_start","message":{"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":2}}}`, in, cacheCreate, cacheRead))
	h.se(fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d,"output_tokens_details":{"thinking_tokens":%d}}}`, in, cacheCreate, cacheRead, out, thinking))
}

func (h *agentHarness) agentCall(id, desc string, background bool) {
	h.se(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"` + id + `","name":"Agent","input":{}}}`)
	h.feed([]byte(fmt.Sprintf(`{"type":"assistant","message":{"model":"claude-haiku-5-5","content":[{"type":"tool_use","id":"%s","name":"Agent","input":{"subagent_type":"general-purpose","description":"%s","run_in_background":%v,"prompt":"Count the files."}}]},"parent_tool_use_id":null}`, id, desc, background)))
	h.se(`{"type":"content_block_stop","index":0}`)
}

func taskStarted(task, tool, desc string, bg bool) string {
	return fmt.Sprintf(`{"type":"system","subtype":"task_started","task_id":"%s","tool_use_id":"%s","description":"%s","subagent_type":"general-purpose","is_backgrounded":%v,"spawn_depth":1,"task_type":"local_agent","prompt":"x"}`, task, tool, desc, bg)
}

func taskProgress(task, tool, activity string, tools int, ms int, summary string) string {
	sum := ""
	if summary != "" {
		sum = `,"summary":"` + summary + `"`
	}
	return fmt.Sprintf(`{"type":"system","subtype":"task_progress","task_id":"%s","tool_use_id":"%s","description":"%s","subagent_type":"general-purpose","usage":{"total_tokens":15970,"tool_uses":%d,"duration_ms":%d},"last_tool_name":"Bash"%s}`, task, tool, activity, tools, ms, sum)
}

func taskNotification(task, tool, status string, tools int, ms int, summary string) string {
	return fmt.Sprintf(`{"type":"system","subtype":"task_notification","task_id":"%s","tool_use_id":"%s","status":"%s","output_file":"/tmp/x.output","summary":"%s","usage":{"total_tokens":18418,"tool_uses":%d,"duration_ms":%d}}`, task, tool, status, summary, tools, ms)
}

// subagentBash: one tool call inside the subagent, plus its own local_bash
// task (which must not get a line).
func (h *agentHarness) subagentBash(parent, task, id string) {
	h.lines(
		`{"type":"assistant","message":{"model":"claude-haiku-5-5","content":[{"type":"tool_use","id":"`+id+`","name":"Bash","input":{"command":"sleep 12","description":"Sleep 12 seconds"}}]},"parent_tool_use_id":"`+parent+`"}`,
		`{"type":"system","subtype":"task_started","task_id":"b`+id+`","owned_by_subagent":true,"tool_use_id":"`+id+`","description":"Sleep 12 seconds","is_backgrounded":false,"parent_task_id":"`+task+`","task_type":"local_bash"}`,
		`{"type":"system","subtype":"task_notification","task_id":"b`+id+`","tool_use_id":"`+id+`","status":"completed","output_file":"","summary":"Sleep 12 seconds"}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"`+id+`","type":"tool_result","content":"(Bash completed with no output)","is_error":false}]},"parent_tool_use_id":"`+parent+`"}`,
	)
}

func (h *agentHarness) finalText(text string) {
	h.se(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	h.se(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + text + `"}}`)
	h.se(`{"type":"content_block_stop","index":0}`)
}

const resultHaiku = `{"type":"result","subtype":"success","result":"%s","num_turns":3,"duration_ms":46779,"session_id":"x","modelUsage":{"claude-haiku-5-5":{"inputTokens":1659,"outputTokens":2628,"costUSD":0.009,"contextWindow":1000000,"maxOutputTokens":128000}}}`

func agentSummaries(evs []map[string]any, id string) []string {
	var out []string
	for _, e := range evs {
		if e["type"] == "tool_end" && e["toolId"] == id {
			out = append(out, e["summary"].(string))
		}
	}
	return out
}

func wantStrings(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestSubagentForegroundLine(t *testing.T) {
	h := newAgentHarness(t, agentDisplay())
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"running"}`)
	h.usage(2, 16017, 4523, 847, 326)
	h.agentCall("toolu_A", "Count scratch files", false)
	h.lines(taskStarted("a1", "toolu_A", "Count scratch files", false))
	h.subagentBash("toolu_A", "a1", "toolu_s1")
	h.advance(2 * time.Second)
	h.lines(taskProgress("a1", "toolu_A", "Running Sleep 12 seconds", 1, 1468, "")) // < 15 s since start: no update
	h.advance(14 * time.Second)
	h.lines(taskProgress("a1", "toolu_A", "Running Count entries", 2, 15311, "")) // 16 s: update
	h.subagentBash("toolu_A", "a1", "toolu_s2")
	h.advance(3 * time.Second)
	h.lines(taskProgress("a1", "toolu_A", "Running Sleep 12 seconds", 3, 16235, "")) // rate-limited
	h.advance(12 * time.Second)
	h.tick() // 15 s since last update and the count changed: update
	h.advance(5 * time.Second)
	h.tick() // nothing changed: no update
	h.lines(taskProgress("a1", "toolu_A", "Waiting 12 seconds in probe-work and some more words", 4, 31057, "Waiting 12 seconds in probe-work and some more words"))
	h.advance(11 * time.Second)
	h.tick()
	h.lines(
		taskNotification("a1", "toolu_A", "completed", 4, 42004, "The count printed **0**."),
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_A","type":"tool_result","content":[{"type":"text","text":"[Subagent hand-back] 0"}]}]},"parent_tool_use_id":null}`,
	)
	h.usage(2, 20540, 6941, 224, 221)
	h.finalText("0")
	h.lines(fmt.Sprintf(resultHaiku, "0"), `{"type":"system","subtype":"session_state_changed","state":"idle"}`)

	evs := h.events()
	wantSig(t, signature(evs), "user_prompt busy tool_start:Agent tool_end:Agent tool_end:Agent tool_end:Agent tool_end:Agent tool_end:Agent text_start text_delta+ text_end text_delta+ result:ok idle")
	wantStrings(t, agentSummaries(evs, "toolu_A"), []string{
		"Agent: Count scratch files",
		"Agent: Count scratch files · 2 tools · 16s",
		"Agent: Count scratch files · 3 tools · 31s",
		"Agent: Count scratch files · 4 tools · 47s · Waiting 12 seconds in probe-work and so…",
		"Agent: Count scratch files · done · 4 tools · 42s",
	})
	final := lastOf(evs, "tool_end")
	detail := final["detail"].(map[string]any)
	if detail["output"] != "The count printed **0**." || detail["input"].(map[string]any)["description"] != "Count scratch files" {
		t.Errorf("final detail %v", detail)
	}
	// footer: as the last text_delta and appended to result.text
	var deltas []string
	for _, e := range evs {
		if e["type"] == "text_delta" {
			deltas = append(deltas, e["text"].(string))
		}
	}
	footer := "— 0s · 1 tool · ctx 3%"
	if deltas[len(deltas)-1] != "\n\n"+footer {
		t.Errorf("footer delta %q", deltas[len(deltas)-1])
	}
	if r := lastOf(evs, "result"); r["text"] != "0\n\n"+footer {
		t.Errorf("result text %q", r["text"])
	}
	// agentProgressSummaries requested at initialize
	h.cur.proc.mu.Lock()
	init := h.cur.proc.fields[0]
	h.cur.proc.mu.Unlock()
	if init["agentProgressSummaries"] != true {
		t.Errorf("initialize fields %v", init)
	}
}

// A background subagent: the Agent tool_result is only the launch ack. After
// the turn nothing is sent while the agent runs; its final line opens
// Claude's autonomous report turn, which gets its own footer.
func TestSubagentBackgroundFinishesAfterTurn(t *testing.T) {
	h := newAgentHarness(t, agentDisplay())
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"running"}`)
	h.usage(2, 16017, 4523, 847, 326)
	h.agentCall("toolu_B", "Background echo", true)
	h.lines(
		`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"b1","task_type":"local_agent","description":"Background echo"}]}`,
		taskStarted("b1", "toolu_B", "Background echo", true),
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_B","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully."}]}]},"parent_tool_use_id":null}`,
	)
	h.usage(2, 20540, 6941, 30, 0)
	h.finalText("started")
	h.lines(fmt.Sprintf(resultHaiku, "started"), `{"type":"system","subtype":"session_state_changed","state":"idle"}`)
	if n := h.m.Lookup(h.sid).mapper.RunningAgents(); n != 1 {
		t.Fatalf("background agent stopped at turn end (running=%d)", n)
	}
	// after the turn: progress, a tick, then the notification
	h.subagentBash("toolu_B", "b1", "toolu_s9")
	h.advance(20 * time.Second)
	h.lines(taskProgress("b1", "toolu_B", "Running Echo BG", 2, 19000, ""))
	h.advance(70 * time.Second)
	h.lines(
		`{"type":"system","subtype":"task_updated","task_id":"b1","patch":{"status":"completed","end_time":1793349191395}}`,
		taskNotification("b1", "toolu_B", "completed", 2, 90000, "BG"),
		`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`,
		`{"type":"system","subtype":"session_state_changed","state":"running"}`,
	)
	h.usage(2, 27000, 300, 12, 0)
	h.finalText("BG done")
	h.lines(fmt.Sprintf(resultHaiku, "BG done"), `{"type":"system","subtype":"session_state_changed","state":"idle"}`)

	evs := h.events()
	wantSig(t, signature(evs), "user_prompt busy tool_start:Agent tool_end:Agent text_start text_delta+ text_end text_delta+ result:ok idle busy tool_end:Agent text_start text_delta+ text_end text_delta+ result:ok idle")
	wantStrings(t, agentSummaries(evs, "toolu_B"), []string{
		"Agent: Background echo",
		"Agent: Background echo · done · 2 tools · 1m30s",
	})
	var results []string
	for _, e := range evs {
		if e["type"] == "result" {
			results = append(results, e["text"].(string))
		}
	}
	// turn 1: one tool (the Agent call); the autonomous turn: none
	wantStrings(t, results, []string{"started\n\n— 0s · 1 tool · ctx 3%", "BG done\n\n— 0s · ctx 3%"})
	if r := lastOf(evs, "tool_end"); r["detail"].(map[string]any)["output"] != "BG" {
		t.Errorf("final output %v", r["detail"])
	}
}

func TestSubagentProgressIntervalZeroAndHeartbeat(t *testing.T) {
	d := agentDisplay()
	d.ProgressInterval = 0
	h := newAgentHarness(t, d)
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"running"}`)
	h.agentCall("toolu_A", "Quiet", false)
	h.lines(taskStarted("a1", "toolu_A", "Quiet", false))
	for i := 1; i <= 5; i++ {
		h.advance(30 * time.Second)
		h.lines(taskProgress("a1", "toolu_A", "x", i, i*30000, ""))
		h.tick()
	}
	h.lines(taskNotification("a1", "toolu_A", "failed", 5, 150000, ""))
	wantStrings(t, agentSummaries(h.events(), "toolu_A"), []string{"Agent: Quiet", "Agent: Quiet · failed · 5 tools · 2m30s"})

	// unchanged line: refreshed only after 4 intervals
	h2 := newAgentHarness(t, agentDisplay())
	h2.lines(`{"type":"system","subtype":"session_state_changed","state":"running"}`)
	h2.agentCall("toolu_A", "Long bash", false)
	h2.lines(taskStarted("a1", "toolu_A", "Long bash", false))
	h2.advance(16 * time.Second)
	h2.lines(taskProgress("a1", "toolu_A", "x", 1, 16000, ""))
	for i := 0; i < 12; i++ {
		h2.advance(5 * time.Second)
		h2.tick()
	}
	wantStrings(t, agentSummaries(h2.events(), "toolu_A"), []string{"Agent: Long bash", "Agent: Long bash · 1 tool · 16s", "Agent: Long bash · 1 tool · 1m16s"})
}

// subagentProgress off: the Agent call is an ordinary tool line again and
// task_* frames emit nothing.
func TestSubagentProgressOff(t *testing.T) {
	d := agentDisplay()
	d.SubagentProgress = false
	h := newAgentHarness(t, d)
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"running"}`)
	h.agentCall("toolu_A", "Count scratch files", false)
	h.lines(taskStarted("a1", "toolu_A", "Count scratch files", false))
	h.advance(20 * time.Second)
	h.lines(taskProgress("a1", "toolu_A", "x", 2, 20000, ""))
	h.lines(taskNotification("a1", "toolu_A", "completed", 2, 20000, "0"),
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_A","type":"tool_result","content":"0"}]},"parent_tool_use_id":null}`)
	h.lines(fmt.Sprintf(resultHaiku, ""), `{"type":"system","subtype":"session_state_changed","state":"idle"}`)
	wantStrings(t, agentSummaries(h.events(), "toolu_A"), []string{"Agent: Count scratch files"})
	h.cur.proc.mu.Lock()
	init := h.cur.proc.fields[0]
	h.cur.proc.mu.Unlock()
	if _, ok := init["agentProgressSummaries"]; ok {
		t.Error("agentProgressSummaries sent with progress off")
	}
}

// Interrupted and failed turns get no footer; neither does /ecc. A turn
// interrupted while a foreground subagent runs ends its line as stopped.
func TestFooterSkippedAndForegroundStopped(t *testing.T) {
	h := newAgentHarness(t, agentDisplay())
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"running"}`)
	h.agentCall("toolu_A", "Slow", false)
	h.lines(taskStarted("a1", "toolu_A", "Slow", false))
	h.m.Lookup(h.sid).Interrupt()
	h.advance(7 * time.Second)
	h.lines(`{"type":"result","subtype":"error_during_execution","terminal_reason":"aborted_tools","num_turns":1,"session_id":"x"}`,
		`{"type":"system","subtype":"session_state_changed","state":"idle"}`)
	evs := h.events()
	wantStrings(t, agentSummaries(evs, "toolu_A"), []string{"Agent: Slow", "Agent: Slow · stopped · 7s"})
	if r := lastOf(evs, "result"); r["text"] != "Interrupted by user" {
		t.Errorf("result %v", r["text"])
	}
	for _, e := range evs {
		if e["type"] == "text_delta" {
			t.Errorf("footer on interrupted turn: %v", e)
		}
	}
	// /ecc footer off: no footer on the next successful turn
	if _, err := h.m.Prompt(h.sid, "/ecc footer off", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Prompt(h.sid, "/ecc agents off", nil); err != nil {
		t.Fatal(err)
	}
	if d := h.m.Display(); d.Footer || d.SubagentProgress {
		t.Fatalf("display %+v", d)
	}
	h.prompt("again")
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"running"}`)
	h.finalText("ok")
	h.lines(fmt.Sprintf(resultHaiku, "ok"), `{"type":"system","subtype":"session_state_changed","state":"idle"}`)
	if r := lastOf(h.events(), "result"); r["text"] != "ok" {
		t.Errorf("footer still on: %q", r["text"])
	}
	var notes []string
	for _, e := range h.events() {
		if e["type"] == "notification" {
			notes = append(notes, e["message"].(string))
		}
	}
	wantStrings(t, notes, []string{"Reply footer: off", "Subagent progress: off"})
}

func TestFmtDurationAndContextPercent(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "0s", 45 * time.Second: "45s", 65 * time.Second: "1m05s", 130*time.Second + 400*time.Millisecond: "2m10s", 62 * time.Minute: "1h02m"} {
		if got := fmtDuration(d); got != want {
			t.Errorf("%v: %q want %q", d, got, want)
		}
	}
	m := NewMapper(func(any) {}, func(string) {}, agentDisplay)
	if _, ok := m.ContextPercent(); ok {
		t.Error("percent without data")
	}
	m.StreamEvent([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":2,"cache_creation_input_tokens":6941,"cache_read_input_tokens":20540,"output_tokens":3}}}`))
	m.StreamEvent([]byte(`{"type":"message_delta","usage":{"output_tokens":224,"output_tokens_details":{"thinking_tokens":221}}}`))
	m.SetMainModel("claude-opus-5-5")
	m.SetContextWindows(map[string]int64{"claude-opus-5-5": 200000, "claude-haiku-5-5": 1000000})
	if p, ok := m.ContextPercent(); !ok || p != 14 { // 27486 / 200000
		t.Errorf("percent %d %v", p, ok)
	}
	if got := m.Footer(130 * time.Second); got != "— 2m10s · ctx 14%" {
		t.Errorf("footer %q", got)
	}
}

// Observed live (2.1.312): with a background agent running, the CLI stays
// "running" after the launch turn's result, and the agent's report comes as a
// second result in the same CLI turn. For the app that is two turns, each
// with its own footer.
func TestSubagentBackgroundSameCLITurn(t *testing.T) {
	h := newAgentHarness(t, agentDisplay())
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"running"}`)
	h.usage(2, 16017, 4523, 847, 326)
	h.agentCall("toolu_B", "Background count", true)
	h.lines(
		taskStarted("b1", "toolu_B", "Background count", true),
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_B","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully."}]}]},"parent_tool_use_id":null}`,
	)
	h.finalText("launched")
	h.lines(fmt.Sprintf(resultHaiku, "launched")) // no idle follows
	h.subagentBash("toolu_B", "b1", "toolu_s9")
	h.advance(16 * time.Second)
	h.lines(taskProgress("b1", "toolu_B", "Running Count", 3, 16000, ""))
	h.advance(9 * time.Second)
	h.lines(taskNotification("b1", "toolu_B", "completed", 4, 25000, "3 and 5"))
	h.usage(2, 27000, 300, 12, 0)
	h.finalText("The background count finished")
	h.lines(fmt.Sprintf(resultHaiku, "The background count finished"), `{"type":"system","subtype":"session_state_changed","state":"idle"}`)

	evs := h.events()
	wantSig(t, signature(evs), "user_prompt busy tool_start:Agent tool_end:Agent text_start text_delta+ text_end text_delta+ result:ok idle busy tool_end:Agent text_start text_delta+ text_end text_delta+ result:ok idle")
	wantStrings(t, agentSummaries(evs, "toolu_B"), []string{
		"Agent: Background count",
		"Agent: Background count · done · 4 tools · 25s",
	})
	var results []string
	for _, e := range evs {
		if e["type"] == "result" {
			results = append(results, e["text"].(string))
		}
	}
	wantStrings(t, results, []string{"launched\n\n— 0s · 1 tool · ctx 2%", "The background count finished\n\n— 0s · ctx 3%"})
}

// The live capture of 2026-10-30 09:58 (session b9f3dc0c): a background agent
// launched, result, then 1m32s of agent progress. The app must see result →
// idle at once, nothing while the agent runs (no running_stats either), then
// busy, the agent's done line, Claude's report, result → idle.
func TestBackgroundAgentNothingWhileIdle(t *testing.T) {
	h := &agentHarness{harness: newHarness(t, agentDisplay()), clock: time.Date(2026, 10, 30, 9, 58, 0, 0, time.UTC)}
	h.m.opts.StatsInterval = 2 * time.Millisecond
	h.prompt("restart the dummy agent")
	h.waitLaunch()
	h.m.Lookup(h.sid).mapper.now = func() time.Time { return h.clock }
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"running"}`)
	h.usage(2, 16017, 4523, 847, 326)
	h.agentCall("toolu_R", "Dummy background agent, retry", true)
	h.lines(
		`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"r1","task_type":"local_agent","description":"Dummy background agent, retry"}]}`,
		taskStarted("r1", "toolu_R", "Dummy background agent, retry", true),
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_R","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully."}]}]},"parent_tool_use_id":null}`,
	)
	h.finalText("restarted it")
	time.Sleep(20 * time.Millisecond)                 // running_stats ticks during the turn
	h.lines(fmt.Sprintf(resultHaiku, "restarted it")) // the CLI stays running: no idle
	evs := h.events()
	if last := evs[len(evs)-1]; last["type"] != "status" || last["state"] != "idle" {
		t.Fatalf("no idle right after the result: last event %v", last)
	}
	if h.m.StatusOf(h.sid) != "idle" {
		t.Fatalf("state %s", h.m.StatusOf(h.sid))
	}
	n := len(evs)

	// 1m32s of agent work while idle: progress, ticks, summaries
	h.subagentBash("toolu_R", "r1", "toolu_s1")
	for i := 1; i <= 18; i++ {
		h.advance(5 * time.Second)
		if i%3 == 0 {
			h.lines(taskProgress("r1", "toolu_R", "Running loop", 1, i*5000, "Looping with date and sleep"))
		}
		h.tick()
	}
	time.Sleep(20 * time.Millisecond)
	h.advance(2 * time.Second)
	h.lines(
		`{"type":"system","subtype":"task_updated","task_id":"r1","patch":{"status":"completed"}}`,
		taskNotification("r1", "toolu_R", "completed", 1, 92000, "Loop finished"),
		`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`,
	)
	if extra := h.events()[n:]; len(extra) != 0 {
		t.Fatalf("events while idle: %v", signature(extra))
	}

	// Claude's autonomous report (the CLI never went idle: no running frame)
	h.se(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
	h.se(`{"type":"content_block_stop","index":0}`)
	h.usage(2, 27000, 300, 12, 0)
	h.finalText("The agent finished")
	h.lines(fmt.Sprintf(resultHaiku, "The agent finished"))
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"idle"}`)
	time.Sleep(20 * time.Millisecond)

	evs = h.events()
	var sig []string
	for _, s := range signature(evs) {
		if s != "running_stats" {
			sig = append(sig, s)
		}
	}
	wantSig(t, sig, "user_prompt busy tool_start:Agent tool_end:Agent text_start text_delta+ text_end text_delta+ result:ok idle busy tool_end:Agent think_start think_end text_start text_delta+ text_end text_delta+ result:ok idle")
	wantStrings(t, agentSummaries(evs, "toolu_R"), []string{
		"Agent: Dummy background agent, retry",
		"Agent: Dummy background agent, retry · done · 1 tool · 1m32s",
	})
	stats := 0
	for i, e := range evs {
		if e["type"] != "running_stats" {
			continue
		}
		stats++
		for _, prev := range evs[:i] {
			if prev["type"] == "result" {
				t.Fatalf("running_stats after a result at %d", i)
			}
		}
	}
	if stats == 0 {
		t.Error("no running_stats during the turn (test clock too fast?)")
	}
	if h.m.StatusOf(h.sid) != "idle" {
		t.Errorf("state %s", h.m.StatusOf(h.sid))
	}
}

// Earlier in the same capture: the first agent ended right after the result
// and Claude went on with no busy and no idle in between. Now: result, idle,
// busy, done line, the new turn.
func TestAutonomousTurnRightAfterResult(t *testing.T) {
	h := newAgentHarness(t, agentDisplay())
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"running"}`)
	h.agentCall("toolu_D", "Dummy background agent for test", true)
	h.lines(
		taskStarted("d1", "toolu_D", "Dummy background agent for test", true),
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_D","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully."}]}]},"parent_tool_use_id":null}`,
	)
	h.finalText("blocks sending")
	h.lines(fmt.Sprintf(resultHaiku, "blocks sending"))
	h.advance(4 * time.Second)
	h.lines(taskNotification("d1", "toolu_D", "completed", 2, 4000, "quit"))
	h.se(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
	h.se(`{"type":"content_block_stop","index":0}`)
	h.finalText("retrying")
	// a result with no main output before it (e.g. a second report) while idle
	h.lines(fmt.Sprintf(resultHaiku, "retrying"))
	h.lines(fmt.Sprintf(resultHaiku, "late report"))
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"idle"}`)
	wantSig(t, signature(h.events()), "user_prompt busy tool_start:Agent tool_end:Agent text_start text_delta+ text_end text_delta+ result:ok idle busy tool_end:Agent think_start think_end text_start text_delta+ text_end text_delta+ result:ok idle busy text_delta+ result:ok idle")
}

// display.agentProgressWhileIdle: the old behaviour (updates between turns).
func TestAgentProgressWhileIdleOption(t *testing.T) {
	d := agentDisplay()
	d.AgentProgressWhileIdle = true
	h := newAgentHarness(t, d)
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"running"}`)
	h.agentCall("toolu_B", "Background echo", true)
	h.lines(
		taskStarted("b1", "toolu_B", "Background echo", true),
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_B","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully."}]}]},"parent_tool_use_id":null}`,
	)
	h.lines(fmt.Sprintf(resultHaiku, "started"))
	h.advance(20 * time.Second)
	h.lines(taskProgress("b1", "toolu_B", "x", 2, 20000, ""))
	h.lines(taskNotification("b1", "toolu_B", "completed", 2, 30000, "BG"))
	h.finalText("BG done")
	h.lines(fmt.Sprintf(resultHaiku, "BG done"), `{"type":"system","subtype":"session_state_changed","state":"idle"}`)
	wantSig(t, signature(h.events()), "user_prompt busy tool_start:Agent tool_end:Agent text_delta+ result:ok idle tool_end:Agent tool_end:Agent busy text_start text_delta+ text_end text_delta+ result:ok idle")
}

// A background agent still running when the user starts the next turn: its
// withheld update goes out inside that turn, after busy.
func TestHeldAgentLineSentInNextTurn(t *testing.T) {
	h := newAgentHarness(t, agentDisplay())
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"running"}`)
	h.agentCall("toolu_B", "Background echo", true)
	h.lines(
		taskStarted("b1", "toolu_B", "Background echo", true),
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_B","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully."}]}]},"parent_tool_use_id":null}`,
	)
	h.lines(fmt.Sprintf(resultHaiku, "started"))
	h.advance(20 * time.Second)
	h.lines(taskProgress("b1", "toolu_B", "x", 2, 20000, "Echoing"))
	h.prompt("what's up")
	h.finalText("still running")
	h.lines(fmt.Sprintf(resultHaiku, "still running"))
	wantSig(t, signature(h.events()), "user_prompt busy tool_start:Agent tool_end:Agent text_delta+ result:ok idle user_prompt busy tool_end:Agent text_start text_delta+ text_end text_delta+ result:ok idle")
	wantStrings(t, agentSummaries(h.events(), "toolu_B"), []string{"Agent: Background echo", "Agent: Background echo · 2 tools · 20s · Echoing"})
}

// /api/interrupt between turns must not reach the CLI (its interrupt stops
// background agents); during a turn it does. initialize declares
// perTaskStopAffordance so the CLI's interrupt spares background agents.
func TestInterruptIdleIsNoop(t *testing.T) {
	h := newAgentHarness(t, agentDisplay())
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"running"}`)
	h.agentCall("toolu_B", "Background echo", true)
	h.lines(taskStarted("b1", "toolu_B", "Background echo", true))
	h.lines(fmt.Sprintf(resultHaiku, "started"))
	reqs := func() string {
		h.cur.proc.mu.Lock()
		defer h.cur.proc.mu.Unlock()
		return strings.Join(h.cur.proc.reqs, ",")
	}
	n := len(h.events())
	h.m.Lookup(h.sid).Interrupt()
	if strings.Contains(reqs(), "interrupt") || len(h.events()) != n {
		t.Fatalf("idle interrupt reached the CLI or the app: reqs=%s events=%v", reqs(), signature(h.events()[n:]))
	}
	h.prompt("next")
	h.lines(`{"type":"system","subtype":"session_state_changed","state":"running"}`)
	h.m.Lookup(h.sid).Interrupt()
	if !strings.Contains(reqs(), "interrupt") {
		t.Fatalf("turn interrupt not sent: %s", reqs())
	}
	h.cur.proc.mu.Lock()
	init := h.cur.proc.fields[0]
	h.cur.proc.mu.Unlock()
	if init["perTaskStopAffordance"] != true {
		t.Errorf("initialize fields %v", init)
	}
}
