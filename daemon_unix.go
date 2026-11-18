//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// startDetached starts cmd in its own session, free of the terminal.
func startDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

func pidAlive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func killPid(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
