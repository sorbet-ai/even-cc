// Package mirror serves terminal sessions to Faceclaw's Terminal app over the
// g2mirror protocol (github.com/jimrandomh/g2mirror, PROTOCOL.md v1): even-cc
// runs a program (Claude Code) in a pseudoconsole, keeps a screen model of it
// for snapshots, and streams its output to the glasses. One process plays
// both g2mirror roles, the gateway and the wrapper, because g2mirror itself
// doesn't run on Windows.
package mirror

import (
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hinshun/vt10x"
)

type ptyProc interface {
	io.ReadWriter
	Resize(cols, rows int) error
	Wait() (int, error)
	Close() error
	Pid() int
}

// attachment is one device connection attached to a session.
type attachment interface {
	send(msg any)
}

type inputJob struct {
	data   []byte
	delays []delay
}

type delay struct {
	At int `json:"at"`
	Ms int `json:"ms"`
}

// Session is one program in a pseudoconsole.
type Session struct {
	ID      string // the protocol's "socket" handle
	Pid     int
	Cwd     string
	Command string
	Preset  string
	started time.Time

	log *slog.Logger
	pty ptyProc
	// callbacks into the server (called without s.mu held)
	onActivity func(s *Session, at int64)
	onBell     func(s *Session, at int64)
	onTitle    func(s *Session, title string)
	onExit     func(s *Session)

	mu        sync.Mutex
	term      vt10x.Terminal
	cols      int
	rows      int
	pend      []byte // an incomplete UTF-8 sequence carried to the next read
	esc       escScanner
	bracketed bool
	title     string
	attached  map[attachment]bool
	viewing   map[attachment]bool
	exited    bool
	exitCode  int

	lastOutput   int64 // unix ms
	lastActivity int64 // last activity notification sent
	lastBell     int64
	bellPending  bool
	bellTimer    *time.Timer
	lastBellSent time.Time

	inputs chan inputJob
	done   chan struct{}
}

func newSession(p ptyProc, cwd, command, preset string, cols, rows int, log *slog.Logger) *Session {
	s := &Session{
		ID:       fmt.Sprintf("%d-%s", p.Pid(), sanitize(cwd)),
		Pid:      p.Pid(),
		Cwd:      cwd,
		Command:  command,
		Preset:   preset,
		started:  time.Now(),
		log:      log,
		pty:      p,
		term:     vt10x.New(vt10x.WithSize(cols, rows)),
		cols:     cols,
		rows:     rows,
		attached: map[attachment]bool{},
		viewing:  map[attachment]bool{},
		inputs:   make(chan inputJob, 64),
		done:     make(chan struct{}),
	}
	return s
}

// sanitize mirrors g2mirror's socket naming: path separators and other
// unsafe characters become underscores.
func sanitize(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (s *Session) run() {
	go s.writeLoop()
	go func() {
		code, err := s.pty.Wait()
		if err != nil {
			s.log.Warn("mirror: wait failed", "session", s.ID, "err", err)
		}
		s.finish(code)
	}()
	buf := make([]byte, 32*1024)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			s.output(buf[:n])
		}
		if err != nil {
			if err != io.EOF {
				s.log.Debug("mirror: output closed", "session", s.ID, "err", err)
			}
			return
		}
	}
}

func (s *Session) output(b []byte) {
	now := time.Now().UnixMilli()
	s.mu.Lock()
	data := append(s.pend, b...)
	cut := len(data)
	// hold back an incomplete trailing rune for the model; viewers get the raw bytes
	for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				cut = i
			}
			break
		}
	}
	if _, err := s.term.Write(data[:cut]); err != nil {
		s.log.Warn("mirror: screen model rejected output", "session", s.ID, "err", err)
	}
	s.pend = append([]byte(nil), data[cut:]...)
	bells, modes := s.esc.scan(b)
	for _, m := range modes {
		if m.mode == 2004 {
			s.bracketed = m.on
		}
	}
	title := s.term.Title()
	titleChanged := title != s.title
	s.title = title
	s.lastOutput = now
	msg := map[string]any{"type": "output", "data": base64.StdEncoding.EncodeToString(b)}
	for v := range s.viewing {
		v.send(msg)
	}
	activity := now-s.lastActivity >= 2000
	if activity {
		s.lastActivity = now
	}
	s.mu.Unlock()

	if titleChanged && s.onTitle != nil {
		s.onTitle(s, title)
		s.broadcastAttached(map[string]any{"type": "title", "title": title})
	}
	if activity && s.onActivity != nil {
		s.onActivity(s, now)
	}
	if bells > 0 {
		s.bell(now)
	}
}

// bell reports at most one bell per 3 s, with a trailing report for bells
// rung inside the window.
func (s *Session) bell(now int64) {
	s.mu.Lock()
	s.lastBell = now
	if time.Since(s.lastBellSent) >= 3*time.Second {
		s.lastBellSent = time.Now()
		s.mu.Unlock()
		if s.onBell != nil {
			s.onBell(s, now)
		}
		return
	}
	if !s.bellPending {
		s.bellPending = true
		wait := 3*time.Second - time.Since(s.lastBellSent)
		s.bellTimer = time.AfterFunc(wait, func() {
			s.mu.Lock()
			s.bellPending = false
			s.lastBellSent = time.Now()
			at := s.lastBell
			s.mu.Unlock()
			if s.onBell != nil {
				s.onBell(s, at)
			}
		})
	}
	s.mu.Unlock()
}

func (s *Session) broadcastAttached(msg any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for a := range s.attached {
		a.send(msg)
	}
}

func (s *Session) finish(code int) {
	s.mu.Lock()
	if s.exited {
		s.mu.Unlock()
		return
	}
	s.exited = true
	s.exitCode = code
	for a := range s.attached {
		a.send(map[string]any{"type": "exit", "status": code})
	}
	if s.bellTimer != nil {
		s.bellTimer.Stop()
	}
	s.mu.Unlock()
	close(s.done)
	s.log.Info("mirror: session ended", "session", s.ID, "status", code)
	go s.pty.Close() // ClosePseudoConsole waits for the output to drain
	if s.onExit != nil {
		s.onExit(s)
	}
}

// connectMsg is the session's greeting to a newly attached device.
func (s *Session) attach(a attachment) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return nil, fmt.Errorf("session %s has exited", s.ID)
	}
	s.attached[a] = true
	msg := map[string]any{
		"type": "connect", "version": 1, "pid": s.Pid, "command": s.Command, "cwd": s.Cwd,
		"host_width": s.cols, "host_height": s.rows, "readonly": false,
		"history":  map[string]int{"next": 0, "oldest": 0},
		"headless": true,
	}
	if s.title != "" {
		msg["title"] = s.title
	}
	if s.Preset != "" {
		msg["launched"] = s.Preset
	}
	if s.lastOutput > 0 {
		msg["last_output_at"] = s.lastOutput
	}
	return msg, nil
}

func (s *Session) detach(a attachment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.attached, a)
	delete(s.viewing, a)
}

// view sizes the program to the viewer and sends it a snapshot; output
// follows. The latest viewer's size wins (in practice there is one: the glasses).
func (s *Session) view(a attachment, cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cols > 0 && rows > 0 && (cols != s.cols || rows != s.rows) {
		s.term.Resize(cols, rows)
		if err := s.pty.Resize(cols, rows); err != nil {
			s.log.Warn("mirror: resize failed", "session", s.ID, "err", err)
		}
		s.cols, s.rows = cols, rows
		// everyone viewing gets the stream at the new size
		snap := s.snapshotLocked()
		for v := range s.viewing {
			if v != a {
				v.send(snap)
			}
		}
	}
	s.viewing[a] = true
	a.send(s.snapshotLocked())
}

func (s *Session) unview(a attachment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.viewing, a)
}

func (s *Session) snapshotLocked() map[string]any {
	return map[string]any{
		"type": "snapshot", "width": s.cols, "height": s.rows, "history_next": 0,
		"data": base64.StdEncoding.EncodeToString(renderScreen(s.term, s.bracketed)),
	}
}

func (s *Session) input(data []byte, delays []delay) error {
	select {
	case <-s.done:
		return fmt.Errorf("session %s has exited", s.ID)
	case s.inputs <- inputJob{data, delays}:
		return nil
	default:
		return fmt.Errorf("session %s input queue is full", s.ID)
	}
}

// writeLoop writes input in order, pausing where the client asked (Claude
// Code reads text plus an immediate "\r" as a paste, so clients pause
// before the Enter).
func (s *Session) writeLoop() {
	for {
		select {
		case <-s.done:
			return
		case job := <-s.inputs:
			off := 0
			for _, d := range job.delays {
				if d.At > off {
					if _, err := s.pty.Write(job.data[off:d.At]); err != nil {
						s.log.Warn("mirror: input write failed", "session", s.ID, "err", err)
						break
					}
					off = d.At
				}
				ms := min(max(d.Ms, 0), 1000)
				select {
				case <-s.done:
					return
				case <-time.After(time.Duration(ms) * time.Millisecond):
				}
			}
			if off < len(job.data) {
				if _, err := s.pty.Write(job.data[off:]); err != nil {
					s.log.Warn("mirror: input write failed", "session", s.ID, "err", err)
				}
			}
		}
	}
}

// info is the session's row in a "sessions" list.
func (s *Session) info() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]any{
		"socket": s.ID, "pid": s.Pid, "cwd_hint": sanitize(s.Cwd),
		"last_bell_at": nullable(s.lastBell), "last_output_at": nullable(s.lastOutput),
		"title": nil,
	}
	if s.title != "" {
		m["title"] = s.title
	}
	if s.Preset != "" {
		m["launched"] = s.Preset
	}
	return m
}

func nullable(ms int64) any {
	if ms == 0 {
		return nil
	}
	return ms
}

func (s *Session) stop() {
	s.pty.Close()
}

// escScanner finds bells (BEL outside an OSC string) and DEC private mode
// changes (CSI ? n h / l) across chunk boundaries.
type escScanner struct {
	state int // 0 ground, 1 ESC, 2 OSC, 3 OSC+ESC, 4 CSI
	csi   []byte
}

type modeChange struct {
	mode int
	on   bool
}

func (e *escScanner) scan(b []byte) (bells int, modes []modeChange) {
	for _, c := range b {
		switch e.state {
		case 0:
			if c == 0x1b {
				e.state = 1
			} else if c == 0x07 {
				bells++
			}
		case 1:
			switch c {
			case ']':
				e.state = 2
			case '[':
				e.state, e.csi = 4, e.csi[:0]
			default:
				e.state = 0
			}
		case 2: // OSC body until BEL or ST
			if c == 0x07 {
				e.state = 0
			} else if c == 0x1b {
				e.state = 3
			}
		case 3:
			if c == '\\' {
				e.state = 0
			} else {
				e.state = 2
			}
		case 4:
			if c >= 0x40 && c <= 0x7e {
				if (c == 'h' || c == 'l') && len(e.csi) > 1 && e.csi[0] == '?' {
					for _, p := range strings.Split(string(e.csi[1:]), ";") {
						var n int
						if _, err := fmt.Sscanf(p, "%d", &n); err == nil {
							modes = append(modes, modeChange{n, c == 'h'})
						}
					}
				}
				e.state = 0
			} else if len(e.csi) < 64 {
				e.csi = append(e.csi, c)
			}
		}
	}
	return bells, modes
}
