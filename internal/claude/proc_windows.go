//go:build windows

package claude

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CREATE_NO_WINDOW keeps a console window from flashing up for claude.exe.
const createNoWindow = 0x08000000

func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow, HideWindow: true}
}

// jobTree puts claude and everything it starts (MCP servers, shells, a
// node.exe behind a .cmd shim) in a Job Object, so terminating the job ends
// the whole tree, and closing the handle (also when even-cc itself dies)
// kills whatever is left.
type jobTree struct {
	job  windows.Handle // 0 = no job (assignment failed): fall back to the process
	proc *os.Process
}

func newProcTree(cmd *exec.Cmd, log *slog.Logger) procTree {
	t := &jobTree{proc: cmd.Process}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		log.Warn("job object unavailable; only the claude process itself can be stopped", "err", err)
		return t
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		log.Warn("job object setup failed", "err", err)
		_ = windows.CloseHandle(job)
		return t
	}
	// A child started between Start and here escapes the job; claude spawns
	// nothing that early.
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		log.Warn("job object: open process failed", "pid", cmd.Process.Pid, "err", err)
		_ = windows.CloseHandle(job)
		return t
	}
	defer windows.CloseHandle(ph)
	if err := windows.AssignProcessToJobObject(job, ph); err != nil {
		log.Warn("job object: assign failed", "pid", cmd.Process.Pid, "err", err)
		_ = windows.CloseHandle(job)
		return t
	}
	t.job = job
	return t
}

// Windows has no SIGTERM; closing stdin already asked claude to exit.
func (t *jobTree) terminate() { t.kill() }

func (t *jobTree) kill() {
	if t.job != 0 {
		_ = windows.TerminateJobObject(t.job, 1)
		return
	}
	_ = t.proc.Kill()
}

func (t *jobTree) release() {
	if t.job != 0 {
		_ = windows.CloseHandle(t.job) // KILL_ON_JOB_CLOSE ends any stragglers
		t.job = 0
	}
}

var shimExe = regexp.MustCompile(`(?i)"%dp0%\\([^"]+\.exe)"`)

// resolveShim maps an npm .cmd shim (claude.cmd) to the executable it runs,
// so claude is started directly: no cmd.exe in between to mangle arguments
// or to outlive a kill.
func resolveShim(path string) string {
	if !strings.EqualFold(filepath.Ext(path), ".cmd") {
		return path
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return path
	}
	m := shimExe.FindSubmatch(b)
	if m == nil {
		return path
	}
	exe := filepath.Join(filepath.Dir(path), string(m[1]))
	if st, err := os.Stat(exe); err != nil || st.IsDir() {
		return path
	}
	return exe
}
