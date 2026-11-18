// Package logfile is a size-capped log file: when a write would take it past
// the cap, it is renamed to <path>.1 (older ones shift to .2 …) and a new
// file is started.
package logfile

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// Defaults: 5 MB per file, 2 old files kept (≤ 15 MB in all).
const (
	DefaultMaxBytes = 5 << 20
	DefaultKeep     = 2
)

// File is safe for concurrent use.
type File struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int
	f    *os.File
	size int64
}

// Open appends to path (created with its directory if needed).
func Open(path string, maxBytes int64, keep int) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &File{path: path, max: maxBytes, keep: keep}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *File) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

// Path is the current file's path.
func (l *File) Path() string { return l.path }

func (l *File) rotate() error {
	l.f.Close()
	l.f = nil
	if l.keep <= 0 {
		_ = os.Remove(l.path)
	} else {
		_ = os.Remove(l.path + "." + strconv.Itoa(l.keep))
		for i := l.keep - 1; i >= 1; i-- {
			_ = os.Rename(l.path+"."+strconv.Itoa(i), l.path+"."+strconv.Itoa(i+1))
		}
		_ = os.Rename(l.path, l.path+".1")
	}
	return l.open()
}

// Write appends p, rotating first when p would take the file past the cap.
func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		if err := l.open(); err != nil {
			return 0, err
		}
	}
	if l.max > 0 && l.size > 0 && l.size+int64(len(p)) > l.max {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// Close closes the file.
func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
