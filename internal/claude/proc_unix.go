//go:build !windows

package claude

import (
	"log/slog"
	"os/exec"
	"syscall"
)

// setProcAttrs starts claude in its own process group, so signals reach
// everything it starts (MCP servers, shells) and a Ctrl-C in even-cc's
// terminal doesn't hit claude before even-cc shuts it down in order.
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setPdeathsig(cmd.SysProcAttr)
}

// groupTree signals claude's process group (pgid == claude's pid).
type groupTree struct{ pgid int }

func newProcTree(cmd *exec.Cmd, _ *slog.Logger) procTree {
	return &groupTree{pgid: cmd.Process.Pid}
}

func (t *groupTree) terminate() { _ = syscall.Kill(-t.pgid, syscall.SIGTERM) }
func (t *groupTree) kill()      { _ = syscall.Kill(-t.pgid, syscall.SIGKILL) }

// release kills what is left of the group after claude exited.
func (t *groupTree) release() { _ = syscall.Kill(-t.pgid, syscall.SIGKILL) }

func resolveShim(path string) string { return path }
