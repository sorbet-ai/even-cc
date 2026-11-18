//go:build windows

package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// startDetached starts cmd with a hidden console of its own (its claude
// children inherit it, so no windows pop up), out of the terminal's process
// group, and out of any job object the terminal runs in when allowed, so
// closing the terminal doesn't end it.
func startDetached(cmd *exec.Cmd) error {
	flags := uint32(windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags | windows.CREATE_BREAKAWAY_FROM_JOB, HideWindow: true}
	if err := cmd.Start(); err == nil {
		return nil
	}
	// the job doesn't allow breaking away: a fresh Cmd (one can't be restarted)
	retry := &exec.Cmd{Path: cmd.Path, Args: cmd.Args, Env: cmd.Env, Dir: cmd.Dir, Stdout: cmd.Stdout, Stderr: cmd.Stderr,
		SysProcAttr: &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}}
	if err := retry.Start(); err != nil {
		return err
	}
	*cmd = *retry //nolint:govet // hand the started process back to the caller
	return nil
}

func pidAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259 // STILL_ACTIVE
}

func killPid(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}
