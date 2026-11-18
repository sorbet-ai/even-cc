//go:build windows

package mirror

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// conPTY runs one program in a Windows pseudoconsole.
type conPTY struct {
	console windows.Handle
	process windows.Handle
	pid     int
	in      *os.File // we write keystrokes here
	out     *os.File // the program's screen output
	closeMu sync.Once
}

func coord(cols, rows int) windows.Coord {
	return windows.Coord{X: int16(cols), Y: int16(rows)}
}

func startPTY(argv []string, dir string, env []string, cols, rows int) (ptyProc, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	// pipes: ours -> console input, console output -> ours
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	var console windows.Handle
	if err := windows.CreatePseudoConsole(coord(cols, rows), windows.Handle(inR.Fd()), windows.Handle(outW.Fd()), 0, &console); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("CreatePseudoConsole: %w", err)
	}
	// the console holds its own handles to its pipe ends
	inR.Close()
	outW.Close()

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		windows.ClosePseudoConsole(console)
		inW.Close()
		outR.Close()
		return nil, err
	}
	defer attrs.Delete()
	// the attribute's value is the HPCON itself, not a pointer to it
	hpc := *(*unsafe.Pointer)(unsafe.Pointer(&console))
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, hpc, unsafe.Sizeof(console)); err != nil {
		windows.ClosePseudoConsole(console)
		inW.Close()
		outR.Close()
		return nil, err
	}
	si := &windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Flags = windows.STARTF_USESTDHANDLES // no inherited std handles: the console provides them

	cmdline, err := windows.UTF16PtrFromString(makeCmdLine(argv))
	if err != nil {
		windows.ClosePseudoConsole(console)
		inW.Close()
		outR.Close()
		return nil, err
	}
	var wdir *uint16
	if dir != "" {
		if wdir, err = windows.UTF16PtrFromString(dir); err != nil {
			windows.ClosePseudoConsole(console)
			inW.Close()
			outR.Close()
			return nil, err
		}
	}
	envBlock, err := makeEnvBlock(env)
	if err != nil {
		windows.ClosePseudoConsole(console)
		inW.Close()
		outR.Close()
		return nil, err
	}
	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := windows.CreateProcess(nil, cmdline, nil, nil, false, flags, envBlock, wdir, &si.StartupInfo, &pi); err != nil {
		windows.ClosePseudoConsole(console)
		inW.Close()
		outR.Close()
		return nil, fmt.Errorf("CreateProcess %s: %w", argv[0], err)
	}
	windows.CloseHandle(pi.Thread)
	return &conPTY{console: console, process: pi.Process, pid: int(pi.ProcessId), in: inW, out: outR}, nil
}

func (p *conPTY) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *conPTY) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *conPTY) Pid() int                    { return p.pid }

func (p *conPTY) Resize(cols, rows int) error {
	return windows.ResizePseudoConsole(p.console, coord(cols, rows))
}

// Wait blocks until the program exits and returns its exit code.
func (p *conPTY) Wait() (int, error) {
	if _, err := windows.WaitForSingleObject(p.process, windows.INFINITE); err != nil {
		return -1, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(p.process, &code); err != nil {
		return -1, err
	}
	return int(code), nil
}

// Close ends the program and the console. ClosePseudoConsole blocks until
// the output pipe is drained, so the reader must still be running.
func (p *conPTY) Close() error {
	p.closeMu.Do(func() {
		windows.TerminateProcess(p.process, 1)
		p.in.Close()
		windows.ClosePseudoConsole(p.console)
		p.out.Close()
		windows.CloseHandle(p.process)
	})
	return nil
}

// makeCmdLine quotes argv the way CommandLineToArgvW parses it.
func makeCmdLine(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = syscall.EscapeArg(a)
	}
	return strings.Join(parts, " ")
}

// makeEnvBlock builds a UTF-16 "k=v\0k=v\0\0" block.
func makeEnvBlock(env []string) (*uint16, error) {
	if len(env) == 0 {
		return nil, nil
	}
	var b []uint16
	for _, kv := range env {
		if strings.IndexByte(kv, 0) >= 0 {
			return nil, fmt.Errorf("environment entry contains NUL: %q", kv)
		}
		b = append(b, windows.StringToUTF16(kv)...) // includes its NUL
	}
	b = append(b, 0)
	return &b[0], nil
}
