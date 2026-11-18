package claude

import _ "embed"

// glassesRules is how to write for the G2 display. The /even skill
// (docs/skills/even/SKILL.md) carries the same text; a test keeps them equal.
//
//go:embed glasses.md
var glassesRules string

// GlassesPrompt is appended to claude's system prompt for sessions even-cc
// runs: those turns come from the glasses, so replies are written for them
// without the user having to run /even. The same session opened at a desk
// (another claude process) keeps normal formatting.
func GlassesPrompt() string {
	return "The user is reading this conversation on Even Realities G2 smart glasses, in Faceclaw's " +
		"Claude Code app (served by even-cc on their machine), and talking to you by voice. " +
		"Write every reply for the glasses, following these rules.\n\n" +
		glassesRules
}
