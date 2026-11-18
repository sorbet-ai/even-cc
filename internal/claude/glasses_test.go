package claude

import (
	"os"
	"strings"
	"testing"
)

// The /even skill and the prompt even-cc appends must carry the same rules.
func TestGlassesRulesMatchSkill(t *testing.T) {
	b, err := os.ReadFile("../../docs/skills/even/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	norm := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	if !strings.Contains(norm(string(b)), norm(glassesRules)) {
		t.Error("docs/skills/even/SKILL.md no longer contains internal/claude/glasses.md verbatim; update one from the other")
	}
	if !strings.HasSuffix(norm(GlassesPrompt()), norm(glassesRules)) || len(glassesRules) < 500 {
		t.Error("GlassesPrompt does not carry the rules")
	}
}

func TestAppendSystemPromptFilePassed(t *testing.T) {
	h := newHarness(t, DisplayFromConfigDefaults())
	h.m.opts.AppendSystemPromptFile = `C:\Users\x\.even-cc\glasses-prompt.md`
	h.prompt("go")
	l := h.waitLaunch()
	if i := indexOf(l.spec.Args, "--append-system-prompt-file"); i < 0 || l.spec.Args[i+1] != h.m.opts.AppendSystemPromptFile {
		t.Errorf("args %v", l.spec.Args)
	}
}

func TestRedundantNotice(t *testing.T) {
	if !redundantNotice("", "Claude needs your permission to use AskUserQuestion") || !redundantNotice("permission_prompt", "x") {
		t.Error("permission notices should be dropped")
	}
	if redundantNotice("idle_prompt", "Claude is waiting for your input") {
		t.Error("other notices should pass")
	}
}
