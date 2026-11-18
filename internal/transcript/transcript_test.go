package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fx struct {
	t    *testing.T
	root string
	s    *Store
}

func newFx(t *testing.T) *fx {
	root := t.TempDir()
	return &fx{t: t, root: root, s: NewStore(root)}
}

func (f *fx) write(project, id string, mtime time.Time, lines ...string) string {
	f.t.Helper()
	dir := filepath.Join(f.root, "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func id(n int) string { return fmt.Sprintf("%08d-0000-4000-8000-000000000000", n) }

func user(uuid, parent, content string) string {
	p := "null"
	if parent != "" {
		p = `"` + parent + `"`
	}
	return fmt.Sprintf(`{"type":"user","uuid":"%s","parentUuid":%s,"cwd":"/work/proj","message":{"role":"user","content":%s}}`, uuid, p, content)
}

func asst(uuid, parent, text string) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":"%s","parentUuid":"%s","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"thinking","thinking":""},{"type":"text","text":%q},{"type":"tool_use","id":"t","name":"Bash","input":{}}]}}`, uuid, parent, text)
}

func TestListTitlesOrderAndSkips(t *testing.T) {
	f := newFx(t)
	base := time.Date(2026, 10, 30, 7, 5, 24, 770_000_000, time.UTC)
	f.write("p", id(1), base, user("u1", "", `"first prompt here"`), `{"type":"custom-title","customTitle":"My Title","sessionId":"x"}`)
	f.write("p", id(2), base, user("u1", "", `"<command-name>/review</command-name>"`), user("u2", "u1", `"<local-command-stdout>x</local-command-stdout>"`))
	f.write("p", id(3), base.Add(-time.Minute), `{"type":"summary","summary":"old style summary"}`, user("u1", "", `"hello"`), `{"type":"ai-title","aiTitle":"AI made this title"}`)
	f.write("p", id(4), base.Add(-2*time.Minute), `{"type":"user","isSidechain":true,"uuid":"s"}`, user("u1", "", `"sidechain"`))
	f.write("p", id(5), base.Add(-3*time.Minute), user("m", "", `"meta"`)[:len(user("m", "", `"meta"`))-1]+`,"isMeta":true}`, user("u1", "", `[{"type":"text","text":"[Request interrupted by user]"},{"type":"text","text":"  line one\nline two  "}]`))
	f.write("p", id(6), base.Add(-4*time.Minute), user("u1", "", `"`+strings.Repeat("é", 70)+`"`))
	f.write("p", id(7), base.Add(-5*time.Minute), `{"type":"queue-operation"}`)
	f.write("p", "not-a-uuid", base.Add(time.Hour), user("u1", "", `"x"`))
	f.write("-q", id(8), base.Add(-6*time.Minute), user("u1", "", `"<bash-input>ls -la</bash-input>"`), `{"type":"last-prompt","lastPrompt":"latest typed prompt"}`)

	list, err := f.s.List(10, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range list {
		got = append(got, s.ID[:8]+"="+s.Title)
	}
	want := []string{
		"00000002=/review",
		"00000001=My Title",
		"00000003=AI made this title",
		"00000005=line one line two",
		"00000006=" + strings.Repeat("é", 64),
		"00000008=latest typed prompt",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	if list[0].Cwd != "/work/proj" || list[0].Mtime != base.UnixMilli() {
		t.Errorf("cwd/mtime %+v", list[0])
	}
	if l, _ := f.s.List(2, ""); len(l) != 2 {
		t.Errorf("limit: %d", len(l))
	}
	if l, _ := f.s.List(10, "/q"); len(l) != 1 || l[0].ID != id(8) {
		t.Errorf("cwd filter: %+v", l)
	}
	// cache: rewrite title, bump mtime -> new title
	f.write("p", id(1), base.Add(time.Hour), user("u1", "", `"first"`), `{"type":"custom-title","customTitle":"Renamed"}`)
	list, _ = f.s.List(1, "")
	if list[0].Title != "Renamed" {
		t.Errorf("cache not invalidated: %q", list[0].Title)
	}
	if f.s.SessionCwd(id(1)) != "/work/proj" || f.s.FindFile(id(99)) != "" {
		t.Error("lookup")
	}
}

func TestHistoryMainChain(t *testing.T) {
	f := newFx(t)
	sid := id(1)
	f.write("p", sid, time.Now(),
		user("u1", "", `"hi"`),
		asst("a1", "u1", "hello"),
		`{"type":"user","uuid":"tr","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"x"}]}}`,
		asst("a2", "tr", "after tool"),
		// abandoned branch from a rewind
		user("b1", "a2", `"branch prompt"`),
		asst("b2", "b1", "branch answer"),
		`{"type":"user","uuid":"m1","parentUuid":"a2","isMeta":true,"message":{"role":"user","content":"meta"}}`,
		user("u2", "m1", `"<task-notification>done</task-notification>"`),
		`{"type":"system","uuid":"sys","parentUuid":"u2","subtype":"turn_duration"}`,
		asst("a3", "u2", "final"),
		`{"type":"assistant","uuid":"side","parentUuid":"a3","isSidechain":true,"message":{"content":[{"type":"text","text":"sidechain"}]}}`,
	)
	h, err := f.s.History(sid, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range h {
		got = append(got, it.Role+":"+it.Text)
	}
	want := "user:hi|assistant:hello|assistant:after tool|user:<task-notification>done</task-notification>|assistant:final"
	if strings.Join(got, "|") != want {
		t.Errorf("got  %s\nwant %s", strings.Join(got, "|"), want)
	}
	h, _ = f.s.History(sid, 2)
	if len(h) != 2 || h[1].Text != "final" {
		t.Errorf("limit 2: %+v", h)
	}
	if h, _ := f.s.History(id(42), 10); len(h) != 0 || h == nil {
		t.Errorf("unknown session: %#v", h)
	}
	if u, _ := f.s.LastUUID(sid); u != "side" {
		t.Errorf("last uuid %q", u)
	}
	if m := f.s.RecentModel(); m != "claude-opus-5-5" {
		t.Errorf("model %q", m)
	}
}

// A chain whose older items sit outside the first 1 MiB tail window.
func TestHistoryGrowsWindow(t *testing.T) {
	f := newFx(t)
	lines := []string{user("u0", "", `"oldest prompt"`)}
	prev := "u0"
	for i := 1; i <= 40; i++ {
		u := fmt.Sprintf("t%d", i)
		lines = append(lines, fmt.Sprintf(`{"type":"user","uuid":"%s","parentUuid":"%s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"%s"}]}}`, u, prev, strings.Repeat("z", 60_000)))
		prev = u
	}
	lines = append(lines, asst("a", prev, "newest"))
	f.write("p", id(1), time.Now(), lines...)
	h, err := f.s.History(id(1), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 || h[0].Text != "oldest prompt" || h[1].Text != "newest" {
		t.Errorf("history %+v", h)
	}
}

func TestFormatModel(t *testing.T) {
	for in, want := range map[string]string{
		"claude-opus-5-5":            "Opus 5.5",
		"claude-sonnet-4-5-20250929": "Sonnet 4.5",
		"claude-haiku-5-5":           "Haiku 5.5",
		"":                           "",
	} {
		if got := FormatModel(in); got != want {
			t.Errorf("%s: %q want %q", in, got, want)
		}
	}
}
