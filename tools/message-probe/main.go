// Sends a cross-session message to a running Claude Code process's inbox:
//
//	go run ./tools/message-probe <pid> <sessionId> <text>
//
// A live check of even-cc's messaging (it writes to that session).
package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"

	"even-cc/internal/claude"
	"even-cc/internal/hub"
	"even-cc/internal/transcript"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: message-probe <pid> <sessionId> <text>")
		os.Exit(2)
	}
	pid, _ := strconv.Atoi(os.Args[1])
	dir := transcript.ClaudeDir()
	m := claude.NewManager(claude.Options{ClaudeDir: dir}, claude.Display{}, hub.New(nil), transcript.NewStore(dir), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.SendToProcess(pid, os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, "failed:", err)
		os.Exit(1)
	}
	fmt.Println("sent")
}
