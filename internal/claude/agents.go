package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"even-cc/internal/events"
)

// agentLine is the single display line of one subagent (Agent tool call),
// keyed by the Agent tool_use id. It is sent as tool_start once, then as
// tool_end on start, on (rate-limited) progress and on finish.
type agentLine struct {
	toolID     string
	name       string
	label      string
	input      json.RawMessage
	background bool
	started    time.Time

	tools   int
	durMs   int64  // CLI-reported run time (task_progress / task_notification)
	summary string // latest agentProgressSummaries text
	status  string // "" while running, then done | failed | stopped
	output  *string

	lastKey  string
	lastEmit time.Time
	// held: an update was withheld while the session was idle (sent at the
	// start of the next turn).
	held bool
}

// taskMsg is the union of the CLI's system task_* frames.
type taskMsg struct {
	Subtype        string  `json:"subtype"`
	TaskID         string  `json:"task_id"`
	ToolUseID      string  `json:"tool_use_id"`
	Description    string  `json:"description"`
	SubagentType   string  `json:"subagent_type"`
	TaskType       string  `json:"task_type"`
	ParentTaskID   string  `json:"parent_task_id"`
	IsBackgrounded *bool   `json:"is_backgrounded"`
	SkipTranscript bool    `json:"skip_transcript"`
	Ambient        bool    `json:"ambient"`
	Status         string  `json:"status"`
	Summary        *string `json:"summary"`
	Usage          *struct {
		ToolUses   int   `json:"tool_uses"`
		DurationMs int64 `json:"duration_ms"`
	} `json:"usage"`
	Patch *struct {
		IsBackgrounded *bool `json:"is_backgrounded"`
	} `json:"patch"`
}

// Task handles a system task_started / task_progress / task_updated /
// task_notification frame. It reports whether a subagent line is running
// (the caller then keeps a ticker going).
func (m *Mapper) Task(raw []byte) bool {
	var t taskMsg
	if json.Unmarshal(raw, &t) != nil {
		return m.RunningAgents() > 0
	}
	switch t.Subtype {
	case "task_started":
		m.taskStarted(t)
	case "task_progress":
		if a := m.agentFor(t); a != nil && a.status == "" {
			if t.Usage != nil {
				a.tools = t.Usage.ToolUses
				a.durMs = t.Usage.DurationMs
			}
			if t.Summary != nil && strings.TrimSpace(*t.Summary) != "" {
				a.summary = oneLine(*t.Summary)
			}
			m.emitAgent(a, false)
		}
	case "task_updated":
		if a := m.agentFor(t); a != nil && t.Patch != nil && t.Patch.IsBackgrounded != nil && *t.Patch.IsBackgrounded {
			a.background = true
		}
	case "task_notification":
		if a := m.agentFor(t); a != nil && a.status == "" {
			if t.Usage != nil {
				a.tools = t.Usage.ToolUses
				a.durMs = t.Usage.DurationMs
			}
			if t.Summary != nil && strings.TrimSpace(*t.Summary) != "" {
				s := *t.Summary
				a.output = &s
			}
			m.finishAgent(a, agentStatus(t.Status))
		}
	}
	return m.RunningAgents() > 0
}

func agentStatus(s string) string {
	switch s {
	case "completed":
		return "done"
	case "failed":
		return "failed"
	}
	return "stopped"
}

func (m *Mapper) agentFor(t taskMsg) *agentLine {
	if id := m.agentTask[t.TaskID]; id != "" {
		return m.agents[id]
	}
	if t.ToolUseID != "" && t.TaskID == "" {
		return m.agents[t.ToolUseID]
	}
	return nil
}

func (m *Mapper) taskStarted(t taskMsg) {
	if !m.disp().agentLines() || t.ToolUseID == "" || t.ParentTaskID != "" || t.SkipTranscript || t.Ambient {
		return
	}
	if t.TaskType != "local_agent" && (t.TaskType != "" || t.SubagentType == "") {
		return // shells, monitors, workflows: shown as their own tool lines
	}
	if a := m.agents[t.ToolUseID]; a != nil {
		m.agentTask[t.TaskID] = t.ToolUseID // resumed run of a known line
		return
	}
	c := m.calls[t.ToolUseID]
	if c == nil || !c.main || c.run != nil {
		return // not a main-agent call we know, or already merged into a run
	}
	in := parseInput(c.input)
	label := t.Description
	if label == "" {
		label = in.str("description")
	}
	if label == "" {
		label = t.SubagentType
	}
	a := &agentLine{
		toolID:     c.id,
		name:       c.name,
		label:      "Agent: " + truncEllipsis(label, 40),
		input:      c.input,
		background: (t.IsBackgrounded != nil && *t.IsBackgrounded) || in["run_in_background"] == true,
		started:    m.now(),
	}
	m.agents[a.toolID] = a
	m.agentTask[t.TaskID] = a.toolID
	if !c.startSent {
		c.startSent = true
		m.emit(events.ToolStart{Type: "tool_start", Name: a.name, ToolID: a.toolID})
	}
	m.emitAgent(a, true)
}

// agentToolResult handles the tool_result of a main-agent call that has a
// subagent line; false when there is none (normal tool_end path).
func (m *Mapper) agentToolResult(c *toolCall) bool {
	a := m.agents[c.id]
	if a == nil {
		return false
	}
	if a.status != "" || a.background {
		return true // already final, or the "async agent launched" ack
	}
	// Foreground agent whose task_notification hasn't come (yet): finish
	// from the tool result.
	status := "done"
	if c.outcome != nil && (c.outcome.isError || c.outcome.denied) {
		status = "failed"
	}
	if a.output == nil && c.output != nil {
		a.output = c.output
	}
	m.finishAgent(a, status)
	return true
}

func (m *Mapper) finishAgent(a *agentLine, status string) {
	a.status = status
	m.emitAgent(a, true)
}

// RunningAgents counts subagent lines that haven't finished.
func (m *Mapper) RunningAgents() int {
	n := 0
	for _, a := range m.agents {
		if a.status == "" {
			n++
		}
	}
	return n
}

// TickAgents sends due progress updates.
func (m *Mapper) TickAgents() {
	for _, a := range m.runningAgents() {
		m.emitAgent(a, false)
	}
}

// StopAgents ends every running subagent line (process gone). With
// foregroundOnly, background agents keep running (end of a turn).
func (m *Mapper) StopAgents(foregroundOnly bool) {
	for _, a := range m.runningAgents() {
		if foregroundOnly && a.background {
			continue
		}
		m.finishAgent(a, "stopped")
	}
}

// runningAgents returns running lines in start order.
func (m *Mapper) runningAgents() []*agentLine {
	var out []*agentLine
	for _, a := range m.agents {
		if a.status == "" {
			out = append(out, a)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].started.Before(out[j-1].started); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// agentHeartbeat: an unchanged line is still refreshed (elapsed time) after
// this many progress intervals.
const agentHeartbeat = 4

// emitAgent sends the line as tool_end. Unforced updates are rate-limited to
// one per ProgressInterval and only sent when the tool count, summary or
// status changed (or after agentHeartbeat intervals).
func (m *Mapper) emitAgent(a *agentLine, force bool) {
	d := m.disp()
	now := m.now()
	key := fmt.Sprintf("%d|%s|%s", a.tools, a.summary, a.status)
	if !force {
		iv := d.ProgressInterval
		if !d.agentLines() || iv <= 0 || now.Sub(a.lastEmit) < iv {
			return
		}
		if key == a.lastKey && now.Sub(a.lastEmit) < agentHeartbeat*iv {
			return
		}
	}
	ev := events.ToolEnd{
		Type:    "tool_end",
		Name:    a.name,
		ToolID:  a.toolID,
		Summary: m.agentText(a, now, a.lastEmit.IsZero()),
		Detail:  events.ToolDetail{Input: m.capInput(a.input, d)},
	}
	if a.status != "" && a.output != nil {
		o := capOutput(*a.output, d.MaxOutput)
		ev.Detail.Output = &o
	}
	if m.quiet || (!m.active && a.background && !d.AgentProgressWhileIdle) {
		// Between turns (or while the app was told idle mid-turn so the user
		// can talk) the app must see nothing: any event makes it show the
		// session as busy again and lock input. The latest state goes out
		// when busy is next sent (usually Claude's report on it).
		a.held = true
		return
	}
	a.held = false
	m.emit(ev)
	a.lastKey = key
	a.lastEmit = now
}

// SetActive tells the mapper whether a turn is running (busy was sent).
func (m *Mapper) SetActive(on bool) { m.active = on }

// SetQuiet holds every subagent update (talk-while-busy idle).
func (m *Mapper) SetQuiet(on bool) { m.quiet = on }

// FlushHeld sends what was withheld while idle: finished lines at once,
// running ones when an update is due.
func (m *Mapper) FlushHeld() {
	var held []*agentLine
	for _, a := range m.agents {
		if a.held {
			held = append(held, a)
		}
	}
	for i := 1; i < len(held); i++ {
		for j := i; j > 0 && held[j].started.Before(held[j-1].started); j-- {
			held[j], held[j-1] = held[j-1], held[j]
		}
	}
	for _, a := range held {
		a.held = false
		m.emitAgent(a, a.status != "")
	}
}

// agentText: "Agent: <desc>" at start, "Agent: <desc> · 12 tools · 45s ·
// <summary>" while running, "Agent: <desc> · done · 14 tools · 1m05s" at the end.
func (m *Mapper) agentText(a *agentLine, now time.Time, first bool) string {
	if first && a.status == "" {
		return a.label
	}
	parts := []string{a.label}
	if a.status != "" {
		parts = append(parts, a.status)
	}
	if a.tools > 0 {
		parts = append(parts, plural(a.tools, "tool", "tools"))
	}
	elapsed := now.Sub(a.started)
	if a.status != "" && a.durMs > 0 {
		elapsed = time.Duration(a.durMs) * time.Millisecond
	}
	parts = append(parts, fmtDuration(elapsed))
	if a.status == "" && a.summary != "" {
		parts = append(parts, truncEllipsis(a.summary, 40))
	}
	return strings.Join(parts, " · ")
}

// fmtDuration: 45s, 1m05s, 1h02m.
func fmtDuration(d time.Duration) string {
	s := int64((d + time.Second/2) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
}

// Footer is the end-of-reply line: "— 2m10s · 4 tools · ctx 62%".
func (m *Mapper) Footer(turn time.Duration) string {
	parts := []string{fmtDuration(turn)}
	if n := m.TurnTools(); n > 0 {
		parts = append(parts, plural(n, "tool", "tools"))
	}
	if p, ok := m.ContextPercent(); ok {
		parts = append(parts, fmt.Sprintf("ctx %d%%", p))
	}
	return "— " + strings.Join(parts, " · ")
}
