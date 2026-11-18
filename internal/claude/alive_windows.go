//go:build windows

package claude

import (
	"errors"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const stillActive = 259

func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return false
	}
	return code == stillActive
}

// processStart is the process creation time as Claude Code writes it to its
// session registry (FILETIME ticks, decimal); "" when unknown.
func processStart(pid int) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return ""
	}
	return strconv.FormatUint(uint64(created.HighDateTime)<<32|uint64(created.LowDateTime), 10)
}

// parentPids maps each running process to its parent.
func parentPids() map[int]int {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	out := map[int]int{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		out[int(e.ProcessID)] = int(e.ParentProcessID)
	}
	return out
}

const pidPlatform = "win32"

func samePidSpace(string) bool { return true }

// stopProcess ends pid and what it started (MCP servers, shells). Windows
// has no polite stop for a console process we don't own, so this is a
// terminate; Claude Code writes its transcript as it goes, so only a reply
// in progress is lost.
func stopProcess(pid int) error {
	parents := parentPids()
	var tree []int // children first
	var walk func(p int)
	walk = func(p int) {
		for child, parent := range parents {
			if parent == p && child != p {
				walk(child)
			}
		}
		tree = append(tree, p)
	}
	walk(pid)
	for _, p := range tree {
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(p))
		if err != nil {
			if p == pid {
				return err
			}
			continue
		}
		_ = windows.TerminateProcess(h, 1)
		windows.CloseHandle(h)
	}
	if waitGone(pid, 5*time.Second) {
		return nil
	}
	return errors.New("it is still running")
}
