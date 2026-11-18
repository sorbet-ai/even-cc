//go:build windows

package claude

import (
	"io"
	"os"
	"time"
)

// inboxConn is a named-pipe client opened as a file.
type inboxConn struct{ *os.File }

func (c inboxConn) SetWriteDeadline(t time.Time) error { return c.File.SetWriteDeadline(t) }

// dialInbox opens a session's messaging named pipe (\.\pipe\...).
func dialInbox(path string, timeout time.Duration) (interface {
	io.WriteCloser
	SetWriteDeadline(time.Time) error
}, error) {
	var f *os.File
	var err error
	for end := time.Now().Add(timeout); ; time.Sleep(50 * time.Millisecond) {
		// a busy pipe (all instances connected) fails with ERROR_PIPE_BUSY: retry
		if f, err = os.OpenFile(path, os.O_RDWR, 0); err == nil || time.Now().After(end) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	return inboxConn{f}, nil
}
