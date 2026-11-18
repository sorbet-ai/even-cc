package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sorbet-ai/even-cc/internal/events"
	"github.com/sorbet-ai/even-cc/internal/jsonx"
)

const testdata = "../../testdata"

// readLines returns the non-empty lines of a file.
func readLines(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		if l := bytes.TrimSpace(sc.Bytes()); len(l) > 0 {
			out = append(out, append([]byte(nil), l...))
		}
	}
	return out
}

// appEvents returns every app event JSON in testdata/even (events/*.jsonl
// and the sse records of turn-*.jsonl), byte-exact as on the wire.
func appEvents(t *testing.T) [][]byte {
	t.Helper()
	var out [][]byte
	files, _ := filepath.Glob(filepath.Join(testdata, "even/events/*.jsonl"))
	for _, f := range files {
		out = append(out, readLines(t, f)...)
	}
	turns, _ := filepath.Glob(filepath.Join(testdata, "even/turn-*.jsonl"))
	for _, f := range turns {
		for _, l := range readLines(t, f) {
			var rec struct {
				Kind string          `json:"kind"`
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(l, &rec); err != nil {
				t.Fatal(err)
			}
			if rec.Kind == "sse" {
				out = append(out, rec.Data)
			}
		}
	}
	if len(out) < 100 {
		t.Fatalf("expected many fixture events, got %d", len(out))
	}
	return out
}

func decodeAs(t *testing.T, raw []byte) any {
	t.Helper()
	var head struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &head)
	var v any
	switch head.Type {
	case "user_prompt":
		v = &events.UserPrompt{}
	case "status":
		v = &events.Status{}
	case "text_delta":
		v = &events.TextDelta{}
	case "tool_start":
		v = &events.ToolStart{}
	case "tool_end":
		v = &events.ToolEnd{}
	case "permission_request":
		v = &events.PermissionRequest{}
	case "permission_result":
		v = &events.PermissionResult{}
	case "user_question":
		v = &events.UserQuestion{}
	case "question_answer":
		v = &events.QuestionAnswer{}
	case "notification":
		v = &events.Notification{}
	case "running_stats":
		v = &events.RunningStats{}
	case "result":
		v = &events.Result{}
	default:
		t.Fatalf("unknown event type %q", head.Type)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Every real event must survive decode -> encode byte for byte: field order,
// omitted keys, escaping and number formatting all match even-terminal.
func TestEventShapesRoundTrip(t *testing.T) {
	for _, raw := range appEvents(t) {
		v := decodeAs(t, raw)
		got := jsonx.Marshal(v)
		if !bytes.Equal(got, raw) {
			t.Errorf("round trip mismatch\n got: %.300s\nwant: %.300s", got, raw)
		}
	}
}

// ClassicSummary must reproduce even-terminal's tool_end summaries exactly.
func TestClassicSummaryMatchesFixtures(t *testing.T) {
	n := 0
	for _, raw := range appEvents(t) {
		var ev events.ToolEnd
		if json.Unmarshal(raw, &ev) != nil || ev.Type != "tool_end" {
			continue
		}
		n++
		if got := ClassicSummary(ev.Name, parseInput(ev.Detail.Input)); got != ev.Summary {
			t.Errorf("%s: got %q want %q", ev.Name, got, ev.Summary)
		}
	}
	if n < 8 {
		t.Fatalf("only %d tool_end fixtures", n)
	}
}

// The permission_request in the Edit turn: description, detail and option
// labels are derived from the Edit input and the suggestions.
func TestClassicPermissionRequestMatchesFixture(t *testing.T) {
	var req events.PermissionRequest
	var input json.RawMessage
	for _, raw := range appEvents(t) {
		var head struct{ Type, ToolID, ToolUseID string }
		_ = json.Unmarshal(raw, &head)
		if head.Type == "permission_request" && bytes.Contains(raw, []byte(`"toolName":"Edit"`)) {
			_ = json.Unmarshal(raw, &req)
		}
	}
	for _, raw := range appEvents(t) {
		var ev events.ToolEnd
		if json.Unmarshal(raw, &ev) == nil && ev.Type == "tool_end" && ev.ToolID == req.ToolUseID {
			input = ev.Detail.Input
		}
	}
	if req.ToolUseID == "" || input == nil {
		t.Fatal("fixture permission_request/tool_end pair not found")
	}
	in := parseInput(input)
	desc := ClassicSummary("Edit", in)
	if desc != req.Description {
		t.Errorf("description %q want %q", desc, req.Description)
	}
	if d := detailFromInput(in); d != req.Detail {
		t.Errorf("detail %q want %q", d, req.Detail)
	}
	opts := permissionOptions(req.Suggestions, desc, false)
	if got, want := jsonx.Marshal(opts), jsonx.Marshal(req.Options); !bytes.Equal(got, want) {
		t.Errorf("options %s want %s", got, want)
	}
	short := permissionOptions(req.Suggestions, desc, true)
	if got := string(jsonx.Marshal(short)); got != `[{"text":"Yes","key":"allow"},{"text":"Accept edits (session)","key":"allowAlways"},{"text":"No","key":"deny"}]` {
		t.Errorf("short options %s", got)
	}
}

func TestPermissionOptionVariants(t *testing.T) {
	cases := []struct {
		sugg, classic, short string
	}{
		{`[{"type":"addDirectories","directories":["/a","/b"],"destination":"projectSettings"}]`, "Yes, and always allow access to `/a`, `/b` for this project", "Always (project)"},
		{`[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":" npm test:* "}],"behavior":"allow","destination":"localSettings"}]`, "Yes, and always allow Bash rule `npm test:*` in local settings", "Always (local)"},
		{`[{"type":"addRules","rules":[{}],"destination":"userSettings"}]`, "Yes, and always allow this tool in user settings", "Always (user)"},
		{`[{"type":"other"}]`, "DESC", "Always (project)"},
		{`[]`, "", ""},
		{`null`, "", ""},
	}
	for _, c := range cases {
		if got := describeSuggestions(json.RawMessage(c.sugg), "DESC", false); got != c.classic {
			t.Errorf("%s classic: %q want %q", c.sugg, got, c.classic)
		}
		if got := describeSuggestions(json.RawMessage(c.sugg), "DESC", true); got != c.short {
			t.Errorf("%s short: %q want %q", c.sugg, got, c.short)
		}
	}
}

func TestStringifyToolResponse(t *testing.T) {
	cases := map[string]string{
		`"plain"`: "plain",
		`[{"type":"tool_reference","tool_name":"TaskStop"}]`:                                   `[{"type":"tool_reference","tool_name":"TaskStop"}]`,
		`{"stdout":"out","stderr":"err"}`:                                                      "out\nerr",
		`{"stdout":"","stderr":"err"}`:                                                         "err",
		`{"content":[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]}`: "a\nb",
		`{"x":1,"a":"<b>"}`:                                                                    `{"x":1,"a":"<b>"}`,
	}
	for in, want := range cases {
		got := stringifyToolResponse(json.RawMessage(in))
		if got == nil || *got != want {
			t.Errorf("%s: got %v want %q", in, got, want)
		}
	}
	if stringifyToolResponse(nil) != nil || stringifyToolResponse(json.RawMessage("null")) != nil {
		t.Error("null content must be omitted")
	}
}

func TestRichSummaries(t *testing.T) {
	bash := parseInput(json.RawMessage(`{"command":"go test ./...","description":"Run unit tests"}`))
	cases := []struct {
		name string
		in   toolInput
		res  *toolOutcome
		want string
	}{
		{"Bash", bash, nil, "Bash: Run unit tests"},
		{"Bash", bash, &toolOutcome{output: "ok\nok\nPASS\n"}, "Bash: Run unit tests · exit 0 · 3 lines"},
		{"Bash", bash, &toolOutcome{output: "Exit code 2\nFAIL", isError: true}, "Bash: Run unit tests · exit 2 · 2 lines"},
		{"Bash", bash, &toolOutcome{output: "User denied", isError: true, denied: true}, "Bash: Run unit tests · denied"},
		{"Edit", parseInput(json.RawMessage(`{"file_path":"/x/api.ts","old_string":"a","new_string":"a\nb\nc"}`)), nil, "Edit api.ts (+3 -1)"},
		{"Read", parseInput(json.RawMessage(`{"file_path":"C:\\src\\main.go","offset":10,"limit":5}`)), &toolOutcome{output: "1\n2\n3\n4\n5"}, "Read main.go (lines 10-15) · 5 lines"},
		{"Grep", parseInput(json.RawMessage(`{"pattern":"TODO"}`)), &toolOutcome{output: "No matches found"}, `Grep "TODO" · no matches`},
		{"Glob", parseInput(json.RawMessage(`{"pattern":"**/*.go"}`)), &toolOutcome{output: "a.go\nb.go"}, "Glob **/*.go · 2 files"},
		{"WebFetch", parseInput(json.RawMessage(`{"url":"https://www.example.com/docs"}`)), nil, "Fetch example.com/docs"},
		{"mcp__github__create_issue", toolInput{}, nil, "MCP github.create_issue"},
	}
	for _, c := range cases {
		if got := RichSummary(c.name, c.in, c.res); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestAutoAllowedBash(t *testing.T) {
	for cmd, want := range map[string]bool{
		"ls -la":            true,
		"git status":        true,
		"git push":          false,
		"ls; rm -rf /":      false,
		"cat a | grep b":    false,
		"echo $(whoami)":    false,
		"  pwd":             true,
		"lsblk":             false,
		"git log --oneline": true,
		"cat x > y":         false,
		"rm file":           false,
	} {
		if got := isAutoAllowedBash(cmd); got != want {
			t.Errorf("%q: got %v want %v", cmd, got, want)
		}
	}
}
