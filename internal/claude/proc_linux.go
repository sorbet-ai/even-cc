//go:build linux

package claude

import "syscall"

// setPdeathsig: claude gets SIGTERM if even-cc dies without shutting down.
func setPdeathsig(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGTERM }
