package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Claude Code lists every running session in <claudeDir>/sessions/<pid>.json.
// A session another process holds (a terminal, VS Code, the desktop app) must
// not get a second writer from here: two processes appending to one
// transcript interleave or fork it. Such sessions are read-only here until
// closed there, or forked into a new session.

// Elsewhere describes another Claude Code process holding a session.
type Elsewhere struct {
	Pid   int    `json:"pid"`
	Where string `json:"where"` // "VS Code", "a terminal", ...
}

type registryEntry struct {
	Pid        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	ProcStart  string `json:"procStart"`
	Kind       string `json:"kind"`
	Entrypoint string `json:"entrypoint"`
	PidDomain  string `json:"pidDomain"`
}

// whereLabel names the client behind a registry entrypoint.
func whereLabel(entrypoint string) string {
	e := strings.ToLower(entrypoint)
	switch {
	case e == "cli":
		return "a terminal"
	case strings.Contains(e, "vscode"):
		return "VS Code"
	case strings.Contains(e, "jetbrains"):
		return "JetBrains"
	case strings.Contains(e, "desktop"):
		return "the desktop app"
	}
	return "another app"
}

// OpenElsewhere maps session ids to the live Claude Code processes, other
// than this server's own, that hold them. Empty without a claude dir.
func (m *Manager) OpenElsewhere() map[string]Elsewhere {
	out := map[string]Elsewhere{}
	if m.opts.ClaudeDir == "" {
		return out
	}
	files, _ := filepath.Glob(filepath.Join(m.opts.ClaudeDir, "sessions", "*.json"))
	if len(files) == 0 {
		return out
	}
	var entries []registryEntry
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var e registryEntry
		if json.Unmarshal(b, &e) != nil || e.Pid <= 0 || e.SessionID == "" {
			continue
		}
		if plat, _, _ := strings.Cut(e.PidDomain, ":"); plat != "" && plat != pidPlatform || !samePidSpace(e.PidDomain) {
			continue // another OS's or pid namespace's pid: can't check it
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return out
	}
	mine := m.ownPids()
	parents := parentPids()
	for _, e := range entries {
		if mine[e.Pid] || mine[parents[e.Pid]] || !processAlive(e.Pid) {
			continue
		}
		if e.ProcStart != "" {
			if st := processStart(e.Pid); st != "" && st != e.ProcStart {
				continue // the pid was reused: a stale entry
			}
		}
		out[e.SessionID] = Elsewhere{Pid: e.Pid, Where: whereLabel(e.Entrypoint)}
	}
	return out
}

// HeldElsewhere reports whether another process holds session id.
func (m *Manager) HeldElsewhere(id string) (Elsewhere, bool) {
	e, ok := m.OpenElsewhere()[id]
	return e, ok
}

// ownPids: this server and the claude processes it runs.
func (m *Manager) ownPids() map[int]bool {
	m.mu.Lock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.Unlock()
	pids := map[int]bool{os.Getpid(): true}
	for _, s := range list {
		s.mu.Lock()
		if s.proc != nil {
			if p := s.proc.Pid(); p > 0 {
				pids[p] = true
			}
		}
		s.mu.Unlock()
	}
	return pids
}

// heldMsg is the 409 text for a prompt to a session held elsewhere.
func heldMsg(e Elsewhere) string {
	return "Open in " + e.Where + ". Close it there, or fork it here."
}

// Release closes the process holding session id elsewhere, so this server
// can continue the session itself. Nothing to do when nobody holds it.
// busyOK: also when that process is mid-turn (else a 409 says so).
func (m *Manager) Release(id string, busyOK bool) error {
	e, held := m.HeldElsewhere(id)
	if !held {
		return nil
	}
	if !busyOK && m.store.TurnOpen(id) {
		return &HTTPError{Code: 409, Msg: "Busy in " + e.Where + ": close it anyway?"}
	}
	m.log.Info("closing the process holding a session", "session", id, "pid", e.Pid, "where", e.Where)
	if err := stopProcess(e.Pid); err != nil {
		return &HTTPError{Code: 500, Msg: "Could not close it in " + e.Where + ": " + err.Error()}
	}
	return nil
}

// waitGone polls until pid has exited, up to d.
func waitGone(pid int, d time.Duration) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if !processAlive(pid) {
			return true
		}
	}
	return !processAlive(pid)
}
