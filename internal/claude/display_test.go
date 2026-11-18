package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// toolTurn feeds three sequential Read calls (each its own API round trip,
// with a thinking block in between), one Bash, then a text block.
func toolTurn(h *harness) {
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"running"}`))
	se := func(ev string) {
		h.feed([]byte(`{"type":"stream_event","event":` + ev + `,"parent_tool_use_id":null}`))
	}
	read := func(id, file, out string) {
		se(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
		se(`{"type":"content_block_stop","index":0}`)
		se(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"` + id + `","name":"Read","input":{}}}`)
		h.feed([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"Read","input":{"file_path":"/src/` + file + `"}}]},"parent_tool_use_id":null}`))
		se(`{"type":"content_block_stop","index":1}`)
		h.feed([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":` + out + `}]},"parent_tool_use_id":null}`))
	}
	read("r1", "a.go", `"1\tpackage a\n2\t"`)
	read("r2", "b.go", `"1\tpackage b"`)
	read("r3", "c.go", `"1\tpackage c"`)
	big := strings.Repeat("x", 5000)
	se(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"b1","name":"Bash","input":{}}}`)
	h.feed([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"go test ./... ` + strings.Repeat("y", 400) + `","description":"Run unit tests"}}]},"parent_tool_use_id":null}`))
	se(`{"type":"content_block_stop","index":0}`)
	h.feed([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"b1","content":"ok\n` + big + `"}]},"parent_tool_use_id":null}`))
	// a subagent tool call (no stream events)
	h.feed([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"s1","name":"Grep","input":{"pattern":"x"}}]},"parent_tool_use_id":"agent1"}`))
	h.feed([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"s1","content":"hit"}]},"parent_tool_use_id":"agent1"}`))
	se(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	se(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`)
	se(`{"type":"content_block_stop","index":0}`)
	h.feed([]byte(`{"type":"result","subtype":"success","result":"done","num_turns":5,"session_id":"x"}`))
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"idle"}`))
}

func runToolTurn(t *testing.T, d Display) (*harness, []map[string]any) {
	h := newHarness(t, d)
	h.prompt("go")
	h.waitLaunch()
	toolTurn(h)
	return h, h.events()
}

func TestDisplayFullClassic(t *testing.T) {
	_, evs := runToolTurn(t, ClassicDisplay())
	wantSig(t, signature(evs), "user_prompt busy think_start think_end tool_start:Read tool_end:Read think_start think_end tool_start:Read tool_end:Read think_start think_end tool_start:Read tool_end:Read tool_start:Bash tool_end:Bash tool_end:Grep text_start text_delta+ text_end result:ok idle")
	bash := lastOf(evs, "tool_end")
	for _, e := range evs {
		if e["type"] == "tool_end" && e["name"] == "Bash" {
			bash = e
		}
	}
	if bash["summary"] != "Bash Run unit tests" || len(bash["detail"].(map[string]any)["output"].(string)) != 5003 {
		t.Errorf("classic must not cap: %v", bash["summary"])
	}
}

func TestDisplayCollapsedRich(t *testing.T) {
	_, evs := runToolTurn(t, DisplayFromConfigDefaults())
	wantSig(t, signature(evs), "user_prompt busy think_start think_end tool_start:Read think_start think_end think_start think_end tool_end:Read tool_start:Bash tool_end:Bash text_start text_delta+ text_end result:ok idle")
	var read, bash map[string]any
	for _, e := range evs {
		if e["type"] == "tool_end" && e["name"] == "Read" {
			read = e
		}
		if e["type"] == "tool_end" && e["name"] == "Bash" {
			bash = e
		}
	}
	if read["summary"] != "Read ×3: a.go, b.go, c.go" || read["toolId"] != "r1" {
		t.Errorf("merged read: %v %v", read["summary"], read["toolId"])
	}
	if in, _ := json.Marshal(read["detail"].(map[string]any)["input"]); !strings.Contains(string(in), `"calls":[{"file_path":"/src/a.go"}`) {
		t.Errorf("merged input %s", in)
	}
	if bash["summary"] != "Bash: Run unit tests · exit 0 · 2 lines" {
		t.Errorf("bash summary %q", bash["summary"])
	}
	out := bash["detail"].(map[string]any)["output"].(string)
	if len(out) > 1600 || !strings.HasSuffix(out, "more bytes]") {
		t.Errorf("output not capped: %d", len(out))
	}
	cmd := bash["detail"].(map[string]any)["input"].(map[string]any)["command"].(string)
	if n := len([]rune(cmd)); n != 301 || !strings.HasSuffix(cmd, "…") {
		t.Errorf("input not capped: %d", n)
	}
}

func TestDisplayHiddenNoThinking(t *testing.T) {
	d := DisplayFromConfigDefaults()
	d.Tools = "hidden"
	d.Thinking = false
	_, evs := runToolTurn(t, d)
	wantSig(t, signature(evs), "user_prompt busy text_start text_delta+ text_end result:ok idle")
}

func TestDisplaySubagentTools(t *testing.T) {
	d := DisplayFromConfigDefaults()
	d.Tools = "full"
	d.SubagentTools = true
	_, evs := runToolTurn(t, d)
	n := 0
	for _, e := range evs {
		if e["type"] == "tool_end" && e["name"] == "Grep" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("subagent tool_end count %d", n)
	}
}

// Parallel calls of different tools: Edit + Bash start, results arrive
// later; each run is flushed once its call finishes.
func TestCollapsedParallelDifferentTools(t *testing.T) {
	h := newHarness(t, DisplayFromConfigDefaults())
	h.prompt("go")
	h.waitLaunch()
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"running"}`))
	for i, name := range []string{"Edit", "Bash"} {
		h.feed([]byte(fmt.Sprintf(`{"type":"stream_event","event":{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"t%d","name":"%s","input":{}}},"parent_tool_use_id":null}`, i, i, name)))
		h.feed([]byte(fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t%d","name":"%s","input":{"file_path":"/x/a.ts","old_string":"a","new_string":"b","command":"ls"}}]},"parent_tool_use_id":null}`, i, name)))
	}
	h.feed([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t0","content":"ok"}]},"parent_tool_use_id":null}`))
	h.feed([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"x"}]},"parent_tool_use_id":null}`))
	h.feed([]byte(`{"type":"result","subtype":"success","result":"","num_turns":2,"session_id":"x"}`))
	h.feed([]byte(`{"type":"system","subtype":"session_state_changed","state":"idle"}`))
	wantSig(t, signature(h.events()), "user_prompt busy tool_start:Edit tool_start:Bash tool_end:Edit tool_end:Bash result:ok idle")
}
