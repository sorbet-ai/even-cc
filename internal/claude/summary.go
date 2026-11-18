package claude

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/sorbet-ai/even-cc/internal/jsonx"
)

// toolInput is a decoded tool input with JS-like accessors.
type toolInput map[string]any

func parseInput(raw json.RawMessage) toolInput {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return toolInput{}
	}
	return toolInput(m)
}

// has: key present and not null (JS `x ?? …` takes x).
func (in toolInput) has(k string) bool { v, ok := in[k]; return ok && v != nil }

// coalesce returns the first present (non-null) key value (JS `??` chain).
func (in toolInput) coalesce(keys ...string) (any, bool) {
	for _, k := range keys {
		if in.has(k) {
			return in[k], true
		}
	}
	return nil, false
}

// str returns the value when it is a string.
func (in toolInput) str(k string) string { s, _ := in[k].(string); return s }

// jsString is JS String(v) for JSON values.
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e21 {
			return strconv.FormatFloat(t, 'f', -1, 64)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = jsString(e)
		}
		return strings.Join(parts, ",")
	default:
		return "[object Object]"
	}
}

// truthy is JS truthiness for JSON values.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	case float64:
		return t != 0 && !math.IsNaN(t)
	default:
		return true
	}
}

// oneLine collapses whitespace runs and trims (summary-format.js oneLine).
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// trunc is summary-format.js truncate(): oneLine + cut to max UTF-16 units + "...".
func trunc(v any, max int) string {
	t := oneLine(jsString(v))
	if t == "" {
		return ""
	}
	if jsonx.UTF16Len(t) > max {
		return jsonx.SliceUTF16(t, max) + "..."
	}
	return t
}

// truncEllipsis is like trunc but with a single "…" (rich summaries).
func truncEllipsis(s string, max int) string {
	t := oneLine(s)
	if jsonx.UTF16Len(t) > max {
		return strings.TrimSpace(jsonx.SliceUTF16(t, max-1)) + "…"
	}
	return t
}

// fileName is summary-format.js fileName().
func fileName(p string) string {
	if p == "" {
		return "file"
	}
	idx := strings.LastIndexAny(p, `/\`)
	name := p[idx+1:]
	if name == "" {
		return p
	}
	return name
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// ClassicSummary is even-terminal's summarizeClaudeToolCall, string for string.
func ClassicSummary(name string, in toolInput) string {
	switch name {
	case "Bash":
		v, ok := in.coalesce("description", "command")
		if !ok {
			v = "command"
		}
		return "Bash " + trunc(v, 50)
	case "Read":
		fn := fileName(in.str("file_path"))
		off, hasOff := in["offset"]
		lim, hasLim := in["limit"]
		if hasOff && hasLim {
			return fmt.Sprintf("Read %s (lines %s-%s)", fn, jsString(off), jsAdd(off, lim))
		}
		if hasLim {
			return fmt.Sprintf("Read %s (%s lines)", fn, jsString(lim))
		}
		return "Read " + fn
	case "Edit":
		fn := fileName(in.str("file_path"))
		added := lineCount(in.str("new_string"))
		removed := lineCount(in.str("old_string"))
		d := added - removed
		switch {
		case d > 0:
			return fmt.Sprintf("Edit %s +%d lines", fn, d)
		case d < 0:
			return fmt.Sprintf("Edit %s %d lines", fn, d)
		}
		return fmt.Sprintf("Edit %s ~%d lines", fn, added)
	case "Write":
		return fmt.Sprintf("Write %s (%d lines)", fileName(in.str("file_path")), lineCount(in.str("content")))
	case "Glob":
		return "Glob " + trunc(in["pattern"], 40)
	case "Grep":
		return `Grep "` + trunc(in["pattern"], 25) + `"`
	case "Agent":
		v, _ := in.coalesce("description")
		return "Agent " + trunc(v, 40)
	case "Skill":
		v, _ := in.coalesce("skill", "name")
		return strings.TrimSpace("Skill " + trunc(jsString(v), 40))
	case "TodoWrite":
		return "TodoWrite update tasks"
	case "WebSearch":
		v, _ := in.coalesce("query")
		return `Search "` + trunc(v, 30) + `"`
	case "WebFetch":
		v, _ := in.coalesce("url")
		return "Fetch " + trunc(v, 40)
	case "ToolSearch":
		return "ToolSearch " + trunc(in["query"], 40)
	case "KillShell":
		v, _ := in.coalesce("pid")
		return strings.TrimSpace("Kill process " + jsString(v))
	case "Config":
		a, _ := in.coalesce("action")
		k, _ := in.coalesce("key")
		return strings.TrimSpace("Config " + jsString(a) + " " + trunc(jsString(k), 30))
	case "Mcp":
		s, _ := in.coalesce("server_name")
		t, _ := in.coalesce("tool_name")
		return "MCP " + jsString(s) + "." + trunc(jsString(t), 30)
	case "RemoteTrigger":
		a, ok := in.coalesce("action")
		if !ok {
			a = "manage"
		}
		return "Trigger " + jsString(a)
	case "NotebookEdit":
		c, ok := in.coalesce("command")
		if !ok {
			c = "edit"
		}
		return "NotebookEdit " + jsString(c)
	case "ExitPlanMode", "ListMcpResources", "AskUserQuestion":
		return name
	case "ReadMcpResource":
		u, _ := in.coalesce("uri")
		return "ReadMcpResource " + trunc(jsString(u), 40)
	case "TaskOutput":
		t, _ := in.coalesce("task_id")
		return strings.TrimSpace("Agent " + trunc(jsString(t), 30))
	case "TaskCreate":
		v, _ := in.coalesce("subject", "description")
		return "Agent " + trunc(jsString(v), 40)
	case "TaskUpdate":
		v, _ := in.coalesce("subject")
		st, _ := in.coalesce("status")
		return strings.TrimSpace("Agent " + trunc(jsString(v), 40) + " " + jsString(st))
	case "TaskGet", "TaskList", "TaskStop":
		return "Agent " + strings.ToLower(strings.Replace(name, "Task", "", 1))
	}
	display := name
	if strings.HasPrefix(name, "Task") {
		display = "Agent"
	}
	for _, k := range []string{"subject", "description", "content", "taskId", "action"} {
		if truthy(in[k]) {
			return display + " " + trunc(jsString(in[k]), 40)
		}
	}
	return display
}

func jsAdd(a, b any) string {
	fa, okA := a.(float64)
	fb, okB := b.(float64)
	if okA && okB {
		return jsString(fa + fb)
	}
	return jsString(a) + jsString(b)
}

// toolOutcome is what we know about a finished tool call.
type toolOutcome struct {
	output  string
	isError bool
	denied  bool
}

var exitCodeRE = regexp.MustCompile(`(?m)^Exit code (\d+)`)

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func shortURL(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	return strings.TrimPrefix(u, "www.")
}

// RichSummary is a more informative one-liner for the 576x288 display. With
// res == nil it describes the call (permission prompts); otherwise it adds
// the outcome (exit code, line counts).
func RichSummary(name string, in toolInput, res *toolOutcome) string {
	var s string
	outLines := 0
	if res != nil {
		outLines = lineCount(strings.TrimRight(res.output, "\n"))
	}
	switch name {
	case "Bash":
		label := in.str("description")
		if label == "" {
			label = in.str("command")
		}
		s = "Bash: " + truncEllipsis(label, 60)
		if res != nil && !res.denied {
			switch {
			case in["run_in_background"] == true && !res.isError:
				s += " · background"
			case res.isError:
				if m := exitCodeRE.FindStringSubmatch(res.output); m != nil {
					s += " · exit " + m[1]
				} else {
					s += " · failed"
				}
			default:
				s += " · exit 0"
			}
			if outLines > 0 && in["run_in_background"] != true {
				s += " · " + plural(outLines, "line", "lines")
			}
		}
	case "Read":
		s = "Read " + fileName(in.str("file_path"))
		if off, ok := in["offset"].(float64); ok {
			if lim, ok := in["limit"].(float64); ok {
				s += fmt.Sprintf(" (lines %s-%s)", jsString(off), jsString(off+lim))
			}
		}
		if res != nil && !res.isError && !res.denied {
			s += " · " + plural(outLines, "line", "lines")
		}
	case "Edit":
		added := lineCount(in.str("new_string"))
		removed := lineCount(in.str("old_string"))
		s = fmt.Sprintf("Edit %s (+%d -%d)", fileName(in.str("file_path")), added, removed)
		if in["replace_all"] == true {
			s = fmt.Sprintf("Edit %s (all, +%d -%d)", fileName(in.str("file_path")), added, removed)
		}
	case "MultiEdit":
		edits, _ := in["edits"].([]any)
		s = fmt.Sprintf("Edit %s (%s)", fileName(in.str("file_path")), plural(len(edits), "edit", "edits"))
	case "Write":
		s = fmt.Sprintf("Write %s (%s)", fileName(in.str("file_path")), plural(lineCount(in.str("content")), "line", "lines"))
	case "Glob":
		s = "Glob " + truncEllipsis(in.str("pattern"), 40)
		if res != nil && !res.isError && !res.denied {
			if strings.HasPrefix(res.output, "No files found") {
				s += " · no files"
			} else {
				s += " · " + plural(outLines, "file", "files")
			}
		}
	case "Grep":
		s = `Grep "` + truncEllipsis(in.str("pattern"), 30) + `"`
		if res != nil && !res.isError && !res.denied {
			if strings.HasPrefix(res.output, "No matches found") || strings.HasPrefix(res.output, "No files found") || outLines == 0 {
				s += " · no matches"
			} else {
				s += " · " + plural(outLines, "line", "lines")
			}
		}
	case "WebFetch":
		s = "Fetch " + truncEllipsis(shortURL(in.str("url")), 50)
	case "WebSearch":
		s = `Search "` + truncEllipsis(in.str("query"), 45) + `"`
	case "Agent", "Task":
		label := in.str("description")
		if label == "" {
			label = in.str("subagent_type")
		}
		s = "Agent: " + truncEllipsis(label, 55)
	case "TodoWrite":
		todos, _ := in["todos"].([]any)
		done := 0
		for _, t := range todos {
			if m, ok := t.(map[string]any); ok && m["status"] == "completed" {
				done++
			}
		}
		s = fmt.Sprintf("Todos %d/%d done", done, len(todos))
	case "AskUserQuestion":
		s = "Question"
		if qs, ok := in["questions"].([]any); ok && len(qs) > 0 {
			if q, ok := qs[0].(map[string]any); ok {
				s = "Question: " + truncEllipsis(jsString(q["question"]), 55)
			}
		}
	case "Skill":
		v, _ := in.coalesce("skill", "name")
		s = "Skill: " + truncEllipsis(jsString(v), 50)
	default:
		if strings.HasPrefix(name, "mcp__") {
			parts := strings.SplitN(strings.TrimPrefix(name, "mcp__"), "__", 2)
			if len(parts) == 2 {
				s = "MCP " + parts[0] + "." + parts[1]
				break
			}
		}
		s = ClassicSummary(name, in)
	}
	if res != nil {
		switch {
		case res.denied:
			s += " · denied"
		case res.isError && name != "Bash":
			s += " · failed"
		}
	}
	return s
}

// callLabel is the short per-call label used in a collapsed run line.
func callLabel(name string, in toolInput) string {
	switch name {
	case "Read", "Edit", "Write", "MultiEdit", "NotebookEdit":
		return fileName(in.str("file_path"))
	case "Bash":
		l := in.str("description")
		if l == "" {
			l = in.str("command")
		}
		return truncEllipsis(l, 40)
	case "Grep":
		return `"` + truncEllipsis(in.str("pattern"), 25) + `"`
	case "Glob":
		return truncEllipsis(in.str("pattern"), 30)
	case "WebFetch":
		return truncEllipsis(shortURL(in.str("url")), 30)
	case "WebSearch":
		return `"` + truncEllipsis(in.str("query"), 25) + `"`
	}
	s := RichSummary(name, in, nil)
	s = strings.TrimPrefix(s, name)
	s = strings.TrimPrefix(s, ":")
	return truncEllipsis(strings.TrimSpace(s), 40)
}

// detailFromInput is mappers.js detailFromInput.
func detailFromInput(in toolInput) string {
	for _, k := range []string{"command", "file_path", "url", "prompt", "query"} {
		if s, ok := in[k].(string); ok && s != "" {
			return jsonx.SliceUTF16(s, 200)
		}
	}
	if s, ok := in["content"].(string); ok && s != "" {
		return jsonx.SliceUTF16(s, 100)
	}
	return ""
}

var autoAllowedBash = regexp.MustCompile(`^\s*(ls|cat|head|tail|wc|pwd|echo|printf|date|whoami|which|where|type|file|stat|du|df|printenv|uname|hostname|id|git\s+(status|log|diff|show|rev-parse))\b`)
var bashControl = regexp.MustCompile("[;&|<>`\r\n]|\\$\\(")

// isAutoAllowedBash is bash-permission.js isAutoAllowedBashCommand.
func isAutoAllowedBash(cmd any) bool {
	c := strings.TrimSpace(jsString(cmd))
	return autoAllowedBash.MatchString(c) && !bashControl.MatchString(c)
}
