//go:build !windows

package mirror

import "errors"

// On Linux and macOS use jimrandomh/g2mirror itself; even-cc's mirror is
// for Windows, where g2mirror doesn't run.
func startPTY(argv []string, dir string, env []string, cols, rows int) (ptyProc, error) {
	return nil, errors.New("terminal mirroring is only built for Windows; use g2mirror on this OS")
}
