//go:build !windows && !linux

package claude

import "syscall"

func setPdeathsig(*syscall.SysProcAttr) {}
