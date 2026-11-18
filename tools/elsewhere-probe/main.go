// Prints the sessions other Claude Code processes hold (a live check of the
// registry reader; reads only).
package main

import (
	"fmt"
	"io"
	"log/slog"

	"even-cc/internal/claude"
	"even-cc/internal/hub"
	"even-cc/internal/transcript"
)

func main() {
	dir := transcript.ClaudeDir()
	m := claude.NewManager(claude.Options{ClaudeDir: dir}, claude.Display{}, hub.New(nil), transcript.NewStore(dir), slog.New(slog.NewTextHandler(io.Discard, nil)))
	for id, e := range m.OpenElsewhere() {
		fmt.Printf("%s pid %d in %s\n", id[:8], e.Pid, e.Where)
	}
	fmt.Println("done")
}
