package mirror

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hinshun/vt10x"
)

// DefaultPort is g2mirror's: Faceclaw connection strings without a port use it.
const DefaultPort = 8737

// Launcher resolves a launch preset (and an optional cwd override) to the
// command to run.
type Launcher func(preset, cwd string) (argv []string, dir string, env []string, err error)

type Server struct {
	Token  string
	Name   string // server_name shown on the glasses
	Log    *slog.Logger
	Launch Launcher

	mu       sync.Mutex
	sessions map[string]*Session
	conns    map[*conn]bool
}

func (s *Server) init() {
	if s.sessions == nil {
		s.sessions = map[string]*Session{}
		s.conns = map[*conn]bool{}
	}
}

// Close ends every session.
func (s *Server) Close() {
	s.mu.Lock()
	var all []*Session
	for _, ss := range s.sessions {
		all = append(all, ss)
	}
	s.mu.Unlock()
	for _, ss := range all {
		ss.stop()
	}
}

// Start runs a preset as a new session sized cols x rows.
func (s *Server) Start(preset, cwd string, cols, rows int) (*Session, error) {
	if s.Launch == nil {
		return nil, errors.New("no launcher configured")
	}
	argv, dir, env, err := s.Launch(preset, cwd)
	if err != nil {
		return nil, err
	}
	if cols <= 0 || rows <= 0 {
		cols, rows = 100, 30
	}
	p, err := startPTY(argv, dir, env, cols, rows)
	if err != nil {
		return nil, err
	}
	ss := newSession(p, dir, strings.Join(argv, " "), preset, cols, rows, s.Log)
	ss.onActivity = func(ss *Session, at int64) {
		s.broadcast(map[string]any{"type": "activity", "socket": ss.ID, "last_output_at": at})
	}
	ss.onBell = func(ss *Session, at int64) {
		s.broadcast(map[string]any{"type": "bell", "socket": ss.ID, "last_bell_at": at})
	}
	ss.onTitle = func(ss *Session, title string) {
		s.broadcast(map[string]any{"type": "title", "socket": ss.ID, "title": title})
	}
	ss.onExit = func(ss *Session) {
		s.mu.Lock()
		delete(s.sessions, ss.ID)
		var attached []*conn
		for c := range s.conns {
			if c.attachedTo() == ss {
				attached = append(attached, c)
			}
		}
		s.mu.Unlock()
		for _, c := range attached {
			c.setSession(nil)
			c.send(map[string]any{"type": "disconnected", "reason": "session closed"})
		}
	}
	s.mu.Lock()
	s.init()
	s.sessions[ss.ID] = ss
	s.mu.Unlock()
	s.Log.Info("mirror: session started", "session", ss.ID, "command", ss.Command, "cwd", dir)
	go ss.run()
	return ss, nil
}

func (s *Server) list() []map[string]any {
	s.mu.Lock()
	all := make([]*Session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		all = append(all, ss)
	}
	s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].started.Before(all[j].started) })
	out := make([]map[string]any, len(all))
	for i, ss := range all {
		out[i] = ss.info()
	}
	return out
}

func (s *Server) broadcast(msg any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		if c.authed() {
			c.send(msg)
		}
	}
}

// conn is one device websocket.
type conn struct {
	out    chan []byte
	cancel context.CancelFunc
	log    *slog.Logger

	mu      sync.Mutex
	isAuthd bool
	cols    int
	rows    int
	device  string
	session *Session
}

func (c *conn) authed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isAuthd
}

func (c *conn) attachedTo() *Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

func (c *conn) setSession(ss *Session) {
	c.mu.Lock()
	c.session = ss
	c.mu.Unlock()
}

// send queues a message; a device that falls this far behind is dropped
// (the stream is a diff, so it can't skip messages).
func (c *conn) send(msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		c.log.Error("mirror: encoding message", "err", err)
		return
	}
	select {
	case c.out <- b:
	default:
		c.log.Warn("mirror: device too slow, dropping it", "device", c.device)
		c.cancel()
	}
}

type clientMsg struct {
	Type      string  `json:"type"`
	Version   int     `json:"version"`
	AuthToken string  `json:"auth_token"`
	Device    string  `json:"device"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Socket    string  `json:"socket"`
	Command   string  `json:"command"`
	Cwd       string  `json:"cwd"`
	Data      string  `json:"data"`
	Delays    []delay `json:"delays"`
	Before    int     `json:"before"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		s.Log.Debug("mirror: websocket accept failed", "remote", r.RemoteAddr, "err", err)
		return
	}
	ws.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c := &conn{out: make(chan []byte, 4096), cancel: cancel, log: s.Log}
	s.mu.Lock()
	s.init()
	s.conns[c] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		if ss := c.attachedTo(); ss != nil {
			ss.detach(c)
		}
	}()

	// writer and keepalive
	go func() {
		ping := time.NewTicker(30 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-c.out:
				wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
				err := ws.Write(wctx, websocket.MessageText, b)
				wcancel()
				if err != nil {
					cancel()
					return
				}
			case <-ping.C:
				pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
				err := ws.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// unauthenticated connections get 10 s
	authTimer := time.AfterFunc(10*time.Second, func() {
		if !c.authed() {
			cancel()
		}
	})
	defer authTimer.Stop()

	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			ws.CloseNow()
			return
		}
		var m clientMsg
		if err := json.Unmarshal(data, &m); err != nil {
			c.send(map[string]any{"type": "error", "message": "invalid JSON"})
			continue
		}
		if !c.authed() {
			if m.Type != "init" {
				refuse(ctx, ws, "expected init")
				return
			}
			if m.Version != 1 {
				refuse(ctx, ws, fmt.Sprintf("unsupported protocol version %d", m.Version))
				return
			}
			if subtle.ConstantTimeCompare([]byte(m.AuthToken), []byte(s.Token)) != 1 {
				s.Log.Warn("mirror: authentication failed", "remote", r.RemoteAddr)
				refuse(ctx, ws, "authentication failed")
				return
			}
			c.mu.Lock()
			c.isAuthd, c.cols, c.rows, c.device = true, m.Width, m.Height, m.Device
			c.mu.Unlock()
			s.Log.Info("mirror: device connected", "device", m.Device, "size", fmt.Sprintf("%dx%d", m.Width, m.Height), "remote", r.RemoteAddr)
			c.send(map[string]any{"type": "init", "version": 1, "readonly": false, "server_name": s.Name})
			continue
		}
		s.handle(c, m)
	}
}

// refuse sends a handshake error, then closes the connection.
func refuse(ctx context.Context, ws *websocket.Conn, message string) {
	b, _ := json.Marshal(map[string]any{"type": "error", "message": message})
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ws.Write(wctx, websocket.MessageText, b); err == nil {
		ws.Close(websocket.StatusPolicyViolation, message)
	} else {
		ws.CloseNow()
	}
}

func (s *Server) handle(c *conn, m clientMsg) {
	switch m.Type {
	case "init":
		c.send(map[string]any{"type": "error", "message": "already initialized"})
	case "list":
		c.send(map[string]any{"type": "sessions", "sessions": s.list()})
	case "connect":
		s.mu.Lock()
		ss := s.sessions[m.Socket]
		s.mu.Unlock()
		if ss == nil {
			c.send(map[string]any{"type": "error", "message": "no such session: " + m.Socket})
			return
		}
		if old := c.attachedTo(); old != nil {
			old.detach(c)
		}
		greeting, err := ss.attach(c)
		if err != nil {
			c.send(map[string]any{"type": "error", "message": err.Error()})
			return
		}
		c.setSession(ss)
		c.send(greeting)
	case "disconnect":
		if ss := c.attachedTo(); ss != nil {
			ss.detach(c)
			c.setSession(nil)
		}
		c.send(map[string]any{"type": "disconnected", "reason": "requested"})
	case "launch":
		preset := m.Command
		if preset == "" {
			preset = "shell"
		}
		c.mu.Lock()
		cols, rows := c.cols, c.rows
		c.mu.Unlock()
		ss, err := s.Start(preset, m.Cwd, cols, rows)
		if err != nil {
			s.Log.Warn("mirror: launch failed", "preset", preset, "err", err)
			c.send(map[string]any{"type": "error", "message": "launch failed: " + err.Error()})
			return
		}
		c.send(map[string]any{"type": "launched", "socket": ss.ID})
	default:
		ss := c.attachedTo()
		if ss == nil {
			if m.Type == "view" || m.Type == "unview" || m.Type == "input" || m.Type == "resize" || m.Type == "history" {
				c.send(map[string]any{"type": "error", "message": "not attached to a session"})
			}
			return
		}
		switch m.Type {
		case "view":
			c.mu.Lock()
			cols, rows := c.cols, c.rows
			c.mu.Unlock()
			ss.view(c, cols, rows)
		case "unview":
			ss.unview(c)
		case "resize":
			c.mu.Lock()
			c.cols, c.rows = m.Width, m.Height
			c.mu.Unlock()
			ss.mu.Lock()
			viewing := ss.viewing[c]
			ss.mu.Unlock()
			if viewing {
				ss.view(c, m.Width, m.Height)
			}
		case "input":
			b, err := base64.StdEncoding.DecodeString(m.Data)
			if err != nil {
				c.send(map[string]any{"type": "error", "message": "input data is not base64"})
				return
			}
			last := 0
			for _, d := range m.Delays {
				if d.At < last || d.At > len(b) || len(m.Delays) > 32 {
					c.send(map[string]any{"type": "error", "message": "invalid input delays"})
					c.cancel()
					return
				}
				last = d.At
			}
			// a submitted line (text, pause, Enter) of key words presses keys
			if len(m.Delays) == 1 && len(b) > 1 && b[len(b)-1] == '\r' && m.Delays[0].At == len(b)-1 {
				ss.mu.Lock()
				appCursor := ss.term.Mode()&vt10x.ModeAppCursor != 0
				ss.mu.Unlock()
				if keys, ok := keysFor(string(b[:len(b)-1]), appCursor); ok {
					s.Log.Info("mirror: key words", "session", ss.ID, "text", string(b[:len(b)-1]), "keys", len(keys))
					b, m.Delays = nil, nil
					for i, k := range keys {
						if i > 0 { // separate reads, so each is one keypress
							m.Delays = append(m.Delays, delay{At: len(b), Ms: 120})
						}
						b = append(b, k...)
					}
				}
			}
			if err := ss.input(b, m.Delays); err != nil {
				c.send(map[string]any{"type": "error", "message": err.Error()})
			}
		case "history":
			// no archive yet: the device keeps what it saw scroll by
			c.send(map[string]any{"type": "history_lines", "start": 0, "oldest": 0, "next": 0, "lines": []any{}})
		}
	}
}
