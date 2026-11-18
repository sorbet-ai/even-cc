package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"even-cc/internal/config"
	"even-cc/internal/events"
	"even-cc/internal/jsonx"
)

// Display controls what the app is sent. Event types and fields never
// change; only text content and which events are sent.
type Display struct {
	Tools         string // full | collapsed | hidden
	Thinking      bool
	Rich          bool
	MaxOutput     int // bytes, 0 = unlimited
	MaxInput      int // UTF-16 units per input string, 0 = unlimited
	ShortOptions  bool
	SubagentTools bool
	// SubagentProgress: one tool line per subagent, updated while it runs.
	SubagentProgress bool
	// ProgressInterval is the minimum gap between updates of one subagent
	// line; 0 = start and finish lines only.
	ProgressInterval time.Duration
	// Footer appends "— <duration> · <n> tools · ctx <p>%" to each reply.
	Footer bool
	// AgentProgressWhileIdle sends background subagent updates between
	// turns too; off, they wait for the next turn.
	AgentProgressWhileIdle bool
}

// ClassicDisplay reproduces even-terminal's output exactly.
func ClassicDisplay() Display {
	return Display{Tools: "full", Thinking: true, SubagentTools: true}
}

// DisplayFromConfig converts the config section.
func DisplayFromConfig(d config.Display) Display {
	return Display{
		Tools:            d.Tools,
		Thinking:         d.Thinking,
		Rich:             d.Summaries == "rich",
		MaxOutput:        d.MaxOutputBytes,
		MaxInput:         d.MaxInputChars,
		ShortOptions:     d.ShortOptions,
		SubagentTools:    d.SubagentTools,
		SubagentProgress: d.SubagentProgress,
		ProgressInterval: time.Duration(d.SubagentProgressSeconds) * time.Second,
		Footer:           d.Footer,

		AgentProgressWhileIdle: d.AgentProgressWhileIdle,
	}
}

// agentLines reports whether subagents get their own progress line.
func (d Display) agentLines() bool { return d.SubagentProgress && d.Tools != "hidden" }

func isAgentTool(name string) bool { return name == "Agent" || name == "Task" }

type toolCall struct {
	id, name string
	input    json.RawMessage
	main     bool
	run      *toolRun
	// startSent: a tool_start with this call's id was sent.
	startSent bool
	done      bool
	output    *string
	outcome   *toolOutcome
}

type toolRun struct {
	name    string
	calls   []*toolCall
	closed  bool
	flushed bool
}

func (r *toolRun) allDone() bool {
	for _, c := range r.calls {
		if !c.done {
			return false
		}
	}
	return true
}

// Mapper turns CLI stdout frames into app events. Not goroutine-safe; the
// owning Session serialises calls.
type Mapper struct {
	emit   func(ev any)
	status func(state string)
	disp   func() Display

	blockType string
	calls     map[string]*toolCall
	runIn     int64
	runOut    int64
	curOut    int64
	run       *toolRun
	waiting   []*toolRun // closed runs whose calls haven't all finished
	denied    map[string]bool

	now        func() time.Time
	agents     map[string]*agentLine // by the Agent tool_use id
	agentTask  map[string]string     // task_id -> tool_use id
	turnTools  map[string]bool       // tool calls counted for the footer
	ctxIn      int64                 // context of the last main-agent API call
	ctxOut     int64
	ctxModel   string
	ctxWindows map[string]int64 // contextWindow per model (from result modelUsage)
	active     bool             // a turn is running (busy was sent)
}

func NewMapper(emit func(ev any), status func(state string), disp func() Display) *Mapper {
	return &Mapper{emit: emit, status: status, disp: disp, calls: map[string]*toolCall{}, denied: map[string]bool{},
		now: time.Now, agents: map[string]*agentLine{}, agentTask: map[string]string{}, turnTools: map[string]bool{}}
}

// ResetTurn clears per-turn state (block tracking, token counters).
func (m *Mapper) ResetTurn() {
	m.blockType = ""
	m.runIn, m.runOut, m.curOut = 0, 0, 0
	m.turnTools = map[string]bool{}
}

// ResetFooter starts a new footer segment (after a result).
func (m *Mapper) ResetFooter() { m.turnTools = map[string]bool{} }

// TurnTools is the number of tool calls in the turn: main-agent calls, plus
// subagent calls when those are shown.
func (m *Mapper) TurnTools() int { return len(m.turnTools) }

// Stats returns running input/output token totals for the turn.
func (m *Mapper) Stats() (int64, int64) { return m.runIn, m.runOut + m.curOut }

// MarkDenied records that the user denied a tool call.
func (m *Mapper) MarkDenied(toolUseID string) { m.denied[toolUseID] = true }

type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	OutputTokensDetails      *struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

func (u *usage) contextIn() int64 {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// contextOut is the output that stays in context (thinking is dropped).
func (u *usage) contextOut() int64 {
	out := u.OutputTokens
	if u.OutputTokensDetails != nil {
		out -= u.OutputTokensDetails.ThinkingTokens
	}
	return max(out, 0)
}

type streamEvent struct {
	Type         string `json:"type"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
	Message *struct {
		Usage *usage `json:"usage"`
	} `json:"message"`
	Usage *usage `json:"usage"`
}

// StreamEvent handles a main-agent stream_event payload.
func (m *Mapper) StreamEvent(raw json.RawMessage) {
	var ev streamEvent
	if json.Unmarshal(raw, &ev) != nil {
		return
	}
	d := m.disp()
	switch ev.Type {
	case "content_block_start":
		if ev.ContentBlock == nil {
			return
		}
		switch ev.ContentBlock.Type {
		case "thinking":
			m.blockType = "thinking"
			if d.Thinking {
				m.status("think_start")
			}
		case "text":
			m.blockType = "text"
			m.CloseRun()
			m.status("text_start")
		case "tool_use":
			m.toolStart(ev.ContentBlock.Name, ev.ContentBlock.ID, d)
		}
	case "content_block_stop":
		if m.blockType != "" {
			bt := m.blockType
			m.blockType = ""
			if bt == "thinking" {
				if d.Thinking {
					m.status("think_end")
				}
			} else {
				m.status("text_end")
			}
		}
	case "content_block_delta":
		if ev.Delta != nil && ev.Delta.Type == "text_delta" {
			m.emit(events.TextDelta{Type: "text_delta", Text: ev.Delta.Text})
		}
	case "message_start":
		if ev.Message != nil && ev.Message.Usage != nil {
			u := ev.Message.Usage
			m.runOut += m.curOut
			m.curOut = 0
			m.runIn += u.contextIn()
			m.ctxIn, m.ctxOut = u.contextIn(), u.contextOut()
		}
	case "message_delta":
		if ev.Usage != nil {
			m.curOut = ev.Usage.OutputTokens
			if in := ev.Usage.contextIn(); in > 0 {
				m.ctxIn = in
			}
			m.ctxOut = ev.Usage.contextOut()
		}
	}
}

// SetMainModel records the model of the latest main-agent message.
func (m *Mapper) SetMainModel(model string) {
	if model != "" {
		m.ctxModel = model
	}
}

// SetContextWindows records each model's context window (result modelUsage).
func (m *Mapper) SetContextWindows(w map[string]int64) {
	if len(w) > 0 {
		m.ctxWindows = w
	}
}

// ContextPercent is the context-window use after the last main-agent API
// call, rounded; ok=false when unknown.
func (m *Mapper) ContextPercent() (int, bool) {
	used := m.ctxIn + m.ctxOut
	if used <= 0 {
		return 0, false
	}
	window := m.ctxWindows[m.ctxModel]
	if window <= 0 && len(m.ctxWindows) == 1 {
		for _, w := range m.ctxWindows {
			window = w
		}
	}
	if window <= 0 {
		return 0, false
	}
	return int((used*100 + window/2) / window), true
}

func (m *Mapper) toolStart(name, id string, d Display) {
	c := m.calls[id]
	if c == nil {
		c = &toolCall{id: id, name: name, main: true}
		m.calls[id] = c
	}
	if isAgentTool(name) && d.agentLines() {
		// Each subagent gets its own line; never merged into a run.
		m.CloseRun()
		c.startSent = true
		m.emit(events.ToolStart{Type: "tool_start", Name: name, ToolID: id})
		return
	}
	switch d.Tools {
	case "hidden":
		return
	case "collapsed":
		if m.run != nil && !m.run.closed && m.run.name == name {
			m.run.calls = append(m.run.calls, c)
			c.run = m.run
			return
		}
		m.CloseRun()
		m.run = &toolRun{name: name, calls: []*toolCall{c}}
		c.run = m.run
	}
	c.startSent = true
	m.emit(events.ToolStart{Type: "tool_start", Name: name, ToolID: id})
}

type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// Assistant records tool_use blocks (main agent and subagents).
func (m *Mapper) Assistant(blocks []contentBlock, parent string) {
	d := m.disp()
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		if parent == "" || (d.SubagentTools && d.Tools != "hidden") {
			m.turnTools[b.ID] = true
		}
		c := m.calls[b.ID]
		if c == nil {
			c = &toolCall{id: b.ID, name: b.Name, main: parent == ""}
			m.calls[b.ID] = c
		}
		c.input = b.Input
	}
}

// ToolResults handles tool_result blocks from a CLI user frame.
func (m *Mapper) ToolResults(blocks []contentBlock) {
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		c := m.calls[b.ToolUseID]
		if c == nil || c.done {
			continue
		}
		c.done = true
		delete(m.calls, b.ToolUseID)
		c.output = stringifyToolResponse(b.Content)
		out := ""
		if c.output != nil {
			out = *c.output
		}
		c.outcome = &toolOutcome{output: out, isError: b.IsError, denied: m.denied[c.id]}
		delete(m.denied, c.id)
		if c.main && m.agentToolResult(c) {
			continue
		}
		m.finish(c)
	}
}

func (m *Mapper) finish(c *toolCall) {
	d := m.disp()
	if !c.main {
		if d.SubagentTools && d.Tools != "hidden" {
			m.emit(m.toolEnd(c, d))
		}
		return
	}
	switch d.Tools {
	case "hidden":
		return
	case "collapsed":
		if c.run != nil {
			if c.run.closed && c.run.allDone() {
				m.flushRun(c.run, d)
			}
			return
		}
	}
	m.emit(m.toolEnd(c, d))
}

// CloseRun ends the current collapsed run; it is sent once all its calls finish.
func (m *Mapper) CloseRun() {
	r := m.run
	if r == nil {
		return
	}
	m.run = nil
	r.closed = true
	if r.allDone() {
		m.flushRun(r, m.disp())
	} else {
		m.waiting = append(m.waiting, r)
	}
}

// FlushAll closes the open run and sends every waiting run with whatever
// calls finished (end of turn).
func (m *Mapper) FlushAll() {
	m.StopAgents(true)
	m.CloseRun()
	d := m.disp()
	for _, r := range m.waiting {
		m.flushRun(r, d)
	}
	m.waiting = nil
}

func (m *Mapper) flushRun(r *toolRun, d Display) {
	if r.flushed {
		return
	}
	r.flushed = true
	var done []*toolCall
	for _, c := range r.calls {
		if c.done {
			done = append(done, c)
		}
	}
	switch len(done) {
	case 0:
		return
	case 1:
		ev := m.toolEnd(done[0], d)
		ev.ToolID = r.calls[0].id
		m.emit(ev)
		return
	}
	m.emit(m.mergedToolEnd(r.name, r.calls[0].id, done, d))
}

func (m *Mapper) capInput(raw json.RawMessage, d Display) json.RawMessage {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	return jsonx.CapStrings(raw, d.MaxInput)
}

func capOutput(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := jsonx.TruncateBytes(s, max)
	return cut + fmt.Sprintf("\n…[%d more bytes]", len(s)-len(cut))
}

func (m *Mapper) summary(c *toolCall, d Display) string {
	in := parseInput(c.input)
	if d.Rich {
		return RichSummary(c.name, in, c.outcome)
	}
	return ClassicSummary(c.name, in)
}

func (m *Mapper) toolEnd(c *toolCall, d Display) events.ToolEnd {
	ev := events.ToolEnd{
		Type:    "tool_end",
		Name:    c.name,
		ToolID:  c.id,
		Summary: m.summary(c, d),
		Detail:  events.ToolDetail{Input: m.capInput(c.input, d)},
	}
	if c.output != nil {
		o := capOutput(*c.output, d.MaxOutput)
		ev.Detail.Output = &o
	}
	return ev
}

func (m *Mapper) mergedToolEnd(name, id string, calls []*toolCall, d Display) events.ToolEnd {
	labels := make([]string, 0, len(calls))
	failed, denied := 0, 0
	var inputs []string
	var out strings.Builder
	for _, c := range calls {
		in := parseInput(c.input)
		labels = append(labels, callLabel(name, in))
		if c.outcome != nil {
			if c.outcome.denied {
				denied++
			} else if c.outcome.isError {
				failed++
			}
		}
		inputs = append(inputs, string(m.capInput(c.input, d)))
		if c.output != nil {
			if out.Len() > 0 {
				out.WriteString("\n")
			}
			out.WriteString("▸ " + callLabel(name, in) + "\n" + *c.output)
		}
	}
	summary := truncEllipsis(fmt.Sprintf("%s ×%d: %s", name, len(calls), strings.Join(labels, ", ")), 90)
	if !d.Rich {
		summary = truncEllipsis(fmt.Sprintf("%s ×%d", ClassicSummary(name, parseInput(calls[0].input)), len(calls)), 90)
	}
	if failed > 0 {
		summary += fmt.Sprintf(" · %d failed", failed)
	}
	if denied > 0 {
		summary += fmt.Sprintf(" · %d denied", denied)
	}
	o := capOutput(out.String(), d.MaxOutput)
	return events.ToolEnd{
		Type:    "tool_end",
		Name:    name,
		ToolID:  id,
		Summary: summary,
		Detail: events.ToolDetail{
			Input:  json.RawMessage(`{"calls":[` + strings.Join(inputs, ",") + `]}`),
			Output: &o,
		},
	}
}

// stringifyToolResponse is mappers.js stringifyToolResponse applied to a
// tool_result block's content. nil when content is absent/null.
func stringifyToolResponse(raw json.RawMessage) *string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return &s
	}
	var obj map[string]json.RawMessage
	if raw[0] == '{' && json.Unmarshal(raw, &obj) == nil {
		var out, errS string
		_, hasOut := obj["stdout"]
		_, hasErr := obj["stderr"]
		var outIsStr, errIsStr bool
		if hasOut {
			outIsStr = json.Unmarshal(obj["stdout"], &out) == nil
		}
		if hasErr {
			errIsStr = json.Unmarshal(obj["stderr"], &errS) == nil
		}
		if outIsStr || errIsStr {
			r := out
			if errS != "" {
				if out != "" {
					r = out + "\n" + errS
				} else {
					r = errS
				}
			}
			return &r
		}
		if c, ok := obj["content"]; ok && len(c) > 0 && c[0] == '[' {
			var blocks []struct {
				Type string `json:"type"`
				Text any    `json:"text"`
			}
			if json.Unmarshal(c, &blocks) == nil {
				parts := []string{}
				for _, b := range blocks {
					if b.Type == "text" {
						parts = append(parts, jsString(b.Text))
					}
				}
				r := strings.Join(parts, "\n")
				return &r
			}
		}
	}
	// JSON.stringify of the parsed value == the CLI's compact bytes.
	r := string(compactJSON(raw))
	return &r
}

func compactJSON(raw json.RawMessage) []byte {
	v, err := jsonx.Parse(raw)
	if err != nil {
		return raw
	}
	b, _ := v.MarshalJSON()
	return b
}
