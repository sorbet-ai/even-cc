//go:build !windows

package claude

import (
	"io"
	"net"
	"time"
)

// dialInbox connects to a session's messaging Unix socket.
func dialInbox(path string, timeout time.Duration) (interface {
	io.WriteCloser
	SetWriteDeadline(time.Time) error
}, error) {
	return net.DialTimeout("unix", path, timeout)
}
