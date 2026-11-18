//go:build !windows

package claude

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// processStart is the process start time as Claude Code writes it to its
// session registry on Linux (/proc/<pid>/stat starttime, in clock ticks);
// "" elsewhere.
func processStart(pid int) string {
	f := procStat(pid)
	if len(f) < 20 {
		return ""
	}
	return f[19] // field 22; f starts at field 3
}

// procStat: /proc/<pid>/stat's fields after the command name.
func procStat(pid int) []string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')') // comm may hold spaces
	if i < 0 {
		return nil
	}
	return strings.Fields(s[i+1:])
}

// samePidSpace: a Linux registry entry names its pid namespace
// ("linux:<machine>:pid:[4026532475]"); pids from another one (an earlier
// WSL boot, a container) mean nothing here.
func samePidSpace(domain string) bool {
	i := strings.Index(domain, "pid:[")
	if i < 0 {
		return true
	}
	ns, err := os.Readlink("/proc/self/ns/pid")
	return err != nil || ns == domain[i:]
}

// parentPids maps each running process to its parent (Linux /proc; empty
// elsewhere, where only pids are matched).
func parentPids() map[int]int {
	out := map[int]int{}
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, p := range stats {
		pid, _ := strconv.Atoi(filepath.Base(filepath.Dir(p)))
		f := procStat(pid) // state ppid ...
		if len(f) < 2 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		out[pid] = ppid
	}
	return out
}

var pidPlatform = runtime.GOOS

// stopProcess asks pid to exit (SIGTERM), then kills it after 5 s.
func stopProcess(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	if waitGone(pid, 5*time.Second) {
		return nil
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if waitGone(pid, 2*time.Second) {
		return nil
	}
	return errors.New("it is still running")
}
