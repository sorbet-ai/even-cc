package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sorbet-ai/even-cc/internal/events"
	"github.com/sorbet-ai/even-cc/internal/guard"
	"github.com/sorbet-ai/even-cc/internal/transcript"
)

const eccTitle = "even-cc"

const eccHelp = "/ecc mode default|auto|plan|acceptEdits · model <name|default> · tools full|collapsed|hidden · thinking on|off · agents on|off · footer on|off · stop · status"

var eccModes = map[string]bool{"default": true, "auto": true, "plan": true, "acceptEdits": true}

// ecc handles a server-side "/ecc …" command. The reply is a notification
// event on the session; nothing is sent to Claude.
func (m *Manager) ecc(sessionID, args string) (string, error) {
	var s *Session
	if sessionID != "" {
		s = m.session(sessionID, true)
		if s == nil {
			return "", &HTTPError{Code: 404, Msg: "Claude session not found: " + sessionID}
		}
	}
	fields := strings.Fields(args)
	cmd, arg := "", ""
	if len(fields) > 0 {
		cmd = strings.ToLower(fields[0])
	}
	if len(fields) > 1 {
		arg = fields[1]
	}
	reply := func(msg string) {
		m.log.Info("/ecc", "cmd", cmd, "arg", arg, "reply", msg)
		if s != nil {
			m.hub.Emit(s.id, events.NewNotification(eccTitle, msg))
		}
	}
	switch cmd {
	case "tools":
		switch arg {
		case "full", "collapsed", "hidden":
			d := m.Display()
			d.Tools = arg
			m.SetDisplay(d)
			reply("Tool display: " + arg)
		default:
			reply("Usage: /ecc tools full|collapsed|hidden")
		}
	case "thinking":
		switch strings.ToLower(arg) {
		case "on", "off":
			d := m.Display()
			d.Thinking = strings.ToLower(arg) == "on"
			m.SetDisplay(d)
			reply("Thinking indicator: " + strings.ToLower(arg))
		default:
			reply("Usage: /ecc thinking on|off")
		}
	case "agents", "footer":
		on := strings.ToLower(arg)
		if on != "on" && on != "off" {
			reply("Usage: /ecc " + cmd + " on|off")
			break
		}
		d := m.Display()
		if cmd == "agents" {
			d.SubagentProgress = on == "on"
			reply("Subagent progress: " + on)
		} else {
			d.Footer = on == "on"
			reply("Reply footer: " + on)
		}
		m.SetDisplay(d)
	case "stop":
		if s == nil {
			return "", &HTTPError{Code: 400, Msg: "/ecc stop needs a sessionId"}
		}
		if s.stop() {
			reply("Stopping the turn")
		} else {
			reply("Nothing to stop (background agents keep running; ask Claude to stop them)")
		}
	case "mode":
		if s == nil {
			return "", &HTTPError{Code: 400, Msg: "/ecc mode needs a sessionId"}
		}
		if !eccModes[arg] {
			reply("Usage: /ecc mode default|auto|plan|acceptEdits")
			break
		}
		go func() { defer guard.Recover(m.log, "/ecc mode"); reply(s.setPermissionMode(arg)) }()
	case "model":
		if s == nil {
			return "", &HTTPError{Code: 400, Msg: "/ecc model needs a sessionId"}
		}
		if arg == "" {
			reply("Usage: /ecc model <name|default>")
			break
		}
		go func() { defer guard.Recover(m.log, "/ecc model"); reply(s.setModel(arg)) }()
	case "status":
		if s == nil {
			d := m.Display()
			reply(fmt.Sprintf("tools %s · thinking %v · agents %s · footer %s", d.Tools, d.Thinking, onOff(d.SubagentProgress), onOff(d.Footer)))
			break
		}
		reply(s.statusText())
	default:
		reply(eccHelp)
	}
	if s == nil {
		return "", nil
	}
	return s.id, nil
}

func (s *Session) liveProc() procHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.launching {
		return nil
	}
	return s.proc
}

func (s *Session) setPermissionMode(mode string) string {
	p := s.liveProc()
	if p == nil {
		s.mu.Lock()
		s.permMode = mode
		s.mu.Unlock()
		return "Permission mode: " + mode + " (from the next prompt)"
	}
	resp, err := p.StartRequest("set_permission_mode", map[string]any{"mode": mode})(10 * time.Second)
	if err != nil {
		return "Mode change failed: " + err.Error()
	}
	var r struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal(resp, &r)
	if r.Mode == "" {
		r.Mode = mode
	}
	s.mu.Lock()
	s.permMode = r.Mode
	s.mu.Unlock()
	return "Permission mode: " + r.Mode
}

func (s *Session) setModel(name string) string {
	if !validModel(name) {
		s.log.Warn("/ecc model: invalid name rejected", "model", name)
		return "Invalid model name: " + truncEllipsis(name, 40)
	}
	var model any = name
	stored := name
	if name == "default" {
		model = "default"
		stored = ""
	}
	p := s.liveProc()
	if p == nil {
		s.mu.Lock()
		s.model = stored
		s.mu.Unlock()
		if stored == "" {
			return "Model: default (from the next prompt)"
		}
		return "Model: " + name + " (from the next prompt)"
	}
	resp, err := p.StartRequest("set_model", map[string]any{"model": model})(15 * time.Second)
	if err != nil {
		return "Model change failed: " + err.Error()
	}
	var r struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(resp, &r)
	s.mu.Lock()
	s.model = stored
	if r.Model != "" {
		s.lastModel = r.Model
	}
	s.mu.Unlock()
	if r.Model != "" {
		return "Model: " + transcript.FormatModel(r.Model)
	}
	return "Model: " + name
}

func (s *Session) statusText() string {
	d := s.m.Display()
	s.mu.Lock()
	defer s.mu.Unlock()
	model := s.lastModel
	if model == "" {
		model = s.model
	}
	if model == "" {
		model = "default"
	} else {
		model = transcript.FormatModel(model)
	}
	proc := "no process"
	if s.proc != nil {
		proc = fmt.Sprintf("warm pid %d", s.proc.Pid())
	}
	state := s.stateLocked()
	mode := s.permMode
	if mode == "" {
		mode = "from settings"
	}
	text := fmt.Sprintf("%s · mode %s · %s · %s · bg tasks %d · tools %s · thinking %v · agents %s · footer %s",
		state, mode, model, proc, len(s.bgTasks), d.Tools, d.Thinking, onOff(d.SubagentProgress), onOff(d.Footer))
	if n := guard.Panics.Load(); n > 0 {
		text += fmt.Sprintf(" · %d internal errors (see log)", n)
	}
	return text
}

// stop interrupts the running turn (/ecc stop); false between turns.
func (s *Session) stop() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.busy {
		return false
	}
	s.log.Info("/ecc stop, interrupting")
	s.interruptLocked()
	return true
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
