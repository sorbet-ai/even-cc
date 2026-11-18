package claude

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"even-cc/internal/events"
	"even-cc/internal/guard"
	"even-cc/internal/hub"
	"even-cc/internal/transcript"
)

// DefaultAutoAllowedTools is even-terminal's list (passed as --allowedTools,
// plus TodoWrite/TaskUpdate approved in the permission callback).
var DefaultAutoAllowedTools = []string{
	"Read", "Glob", "Grep", "ToolSearch",
	"WebSearch", "WebFetch", "TaskOutput", "ExitPlanMode",
	"ListMcpResources", "ReadMcpResource",
}

var callbackManaged = map[string]bool{"TodoWrite": true, "TaskUpdate": true}

// Options configure how sessions launch claude.
type Options struct {
	ClaudePath     string
	SettingSources string
	PermissionMode string
	Model          string
	AllowedTools   []string // nil = DefaultAutoAllowedTools + TodoWrite, TaskUpdate
	MaxTurns       int
	ExtraArgs      []string
	// AppendSystemPromptFile is passed as --append-system-prompt-file ("" = none).
	AppendSystemPromptFile string
	IdleTimeout            time.Duration
	PermissionTimeout      time.Duration
	QuestionTimeout        time.Duration
	DefaultCwd             string
	StatsInterval          time.Duration
	AgentTick              time.Duration // subagent line refresh check; 0 = 5 s, <0 = off
	// ResultIdleGrace: end the turn this long after a result when the CLI
	// sends no idle and the main agent stays silent (0 = 5 s).
	ResultIdleGrace time.Duration
	Env             []string // base env (defaults to os.Environ())
	// ClaudeDir holds Claude Code's live-session registry; sessions other
	// processes hold are read-only here ("" = don't check).
	ClaudeDir string
}

type launcherFunc func(spec ProcSpec, onLine func([]byte), onExit func(procHandle)) (procHandle, error)

// Manager owns all sessions this server has run.
type Manager struct {
	opts    Options
	hub     *hub.Hub
	store   *transcript.Store
	log     *slog.Logger
	launch  launcherFunc
	display atomic.Pointer[Display]

	autoAllowed map[string]bool
	sdkAllowed  []string

	// onInitialize receives each process's initialize response (for /api/info).
	onInitialize func(json.RawMessage)

	mu       sync.Mutex
	sessions map[string]*Session

	// closed is set when Shutdown starts: no claude process starts after it.
	closed atomic.Bool

	usageMu sync.Mutex
	usage   *Usage
}

// NewManager builds a manager launching real claude processes.
func NewManager(opts Options, d Display, h *hub.Hub, store *transcript.Store, log *slog.Logger) *Manager {
	m := &Manager{opts: opts, hub: h, store: store, log: log, sessions: map[string]*Session{}}
	m.display.Store(&d)
	if m.opts.StatsInterval == 0 {
		m.opts.StatsInterval = 10 * time.Second
	}
	if m.opts.ResultIdleGrace == 0 {
		m.opts.ResultIdleGrace = 5 * time.Second
	}
	if m.opts.AgentTick == 0 {
		m.opts.AgentTick = 5 * time.Second
	}
	if m.opts.PermissionTimeout == 0 {
		m.opts.PermissionTimeout = 10 * time.Minute
	}
	if m.opts.QuestionTimeout == 0 {
		m.opts.QuestionTimeout = 10 * time.Minute
	}
	if m.opts.Env == nil {
		m.opts.Env = os.Environ()
	}
	auto := opts.AllowedTools
	if auto == nil {
		auto = append(append([]string{}, DefaultAutoAllowedTools...), "TodoWrite", "TaskUpdate")
	}
	m.autoAllowed = map[string]bool{}
	for _, t := range auto {
		m.autoAllowed[t] = true
		if t != "AskUserQuestion" && !callbackManaged[t] {
			m.sdkAllowed = append(m.sdkAllowed, t)
		}
	}
	m.launch = func(spec ProcSpec, onLine func([]byte), onExit func(procHandle)) (procHandle, error) {
		l := spec.Log
		if l == nil {
			l = log
		}
		p, err := StartProc(spec, l, onLine, func(p *Proc) { onExit(p) })
		if err != nil {
			return nil, err
		}
		return p, nil
	}
	return m
}

// SetInitializeHook registers a callback for initialize responses.
func (m *Manager) SetInitializeHook(f func(json.RawMessage)) { m.onInitialize = f }

// Display returns the current display settings.
func (m *Manager) Display() Display { return *m.display.Load() }

// SetDisplay replaces the display settings at runtime.
func (m *Manager) SetDisplay(d Display) { m.display.Store(&d) }

func (m *Manager) procSpec(s *Session) ProcSpec {
	args := []string{
		"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--include-partial-messages", "--permission-prompt-tool", "stdio",
	}
	if m.opts.SettingSources != "" {
		args = append(args, "--setting-sources", m.opts.SettingSources)
	}
	if s.permMode != "" { // "" = the user's permissions.defaultMode
		args = append(args, "--permission-mode", s.permMode)
	}
	if len(m.sdkAllowed) > 0 {
		args = append(args, "--allowedTools", strings.Join(m.sdkAllowed, ","))
	}
	if s.model != "" && validModel(s.model) {
		args = append(args, "--model", s.model)
	} else if s.model != "" {
		s.log.Warn("ignoring invalid model name", "model", s.model)
	}
	if m.opts.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(m.opts.MaxTurns))
	}
	if m.store.FindFile(s.id) != "" {
		args = append(args, "--resume", s.id)
	} else if s.forkFrom != "" {
		args = append(args, "--resume", s.forkFrom, "--fork-session", "--session-id", s.id)
	} else {
		args = append(args, "--session-id", s.id)
	}
	if m.opts.AppendSystemPromptFile != "" {
		args = append(args, "--append-system-prompt-file", m.opts.AppendSystemPromptFile)
	}
	args = append(args, m.opts.ExtraArgs...)
	return ProcSpec{Path: m.opts.ClaudePath, Args: args, Dir: s.cwd, Env: ChildEnv(m.opts.Env), Log: s.log}
}

// modelRE: model names and aliases (opus, claude-opus-5-5[1m], Bedrock ids
// and ARNs). Anything else could smuggle arguments into the command line.
var modelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@\[\]-]{0,199}$`)

func validModel(name string) bool { return modelRE.MatchString(name) }

// HTTPError carries an HTTP status for route handlers.
type HTTPError struct {
	Code int
	Msg  string
}

func (e *HTTPError) Error() string { return e.Msg }

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// session returns the in-memory session for id, loading it from its
// transcript when needed. nil when the id is unknown.
func (m *Manager) session(id string, create bool) *Session {
	if s := m.Lookup(id); s != nil || !create {
		return s
	}
	// Disk scans run without m.mu, so other requests aren't held up.
	if m.store.FindFile(id) == "" {
		return nil
	}
	cwd := m.store.SessionCwd(id)
	if cwd == "" {
		cwd = m.opts.DefaultCwd
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[id]; s != nil { // created meanwhile
		return s
	}
	s := newSession(m, id, cwd)
	m.sessions[id] = s
	return s
}

// Prompt implements POST /api/prompt. Returns the session id.
func (m *Manager) Prompt(sessionID, text string, cwd *string) (string, error) {
	if text == "/ecc" || strings.HasPrefix(text, "/ecc ") {
		return m.ecc(sessionID, strings.TrimSpace(strings.TrimPrefix(text, "/ecc")))
	}
	var s *Session
	if sessionID != "" {
		if e, held := m.HeldElsewhere(sessionID); held {
			return "", &HTTPError{Code: 409, Msg: heldMsg(e)}
		}
		s = m.session(sessionID, true)
		if s == nil {
			return "", &HTTPError{Code: 404, Msg: "Claude session not found: " + sessionID}
		}
	} else {
		dir := m.opts.DefaultCwd
		if cwd != nil {
			dir = *cwd
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			wd, _ := os.Getwd()
			m.log.Warn("cwd not found, using server cwd", "cwd", dir, "fallback", wd)
			dir = wd
		}
		id := newUUID()
		s = newSession(m, id, dir)
		m.mu.Lock()
		m.sessions[id] = s
		m.mu.Unlock()
		m.log.Info("new session", "session", id, "cwd", dir)
	}
	m.hub.Emit(s.id, events.NewUserPrompt(text))
	s.Submit(text)
	return s.id, nil
}

// Fork starts a new session continuing sourceID's conversation (the source
// stays as it is: another process may hold it) and sends text to it.
// Returns the new session's id.
func (m *Manager) Fork(sourceID, text string) (string, error) {
	if !transcript.IsSessionID(sourceID) || m.store.FindFile(sourceID) == "" {
		return "", &HTTPError{Code: 404, Msg: "Claude session not found: " + sourceID}
	}
	cwd := m.store.SessionCwd(sourceID)
	if cwd == "" {
		cwd = m.opts.DefaultCwd
	}
	id := newUUID()
	s := newSession(m, id, cwd)
	s.forkFrom = sourceID
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	m.log.Info("forked session", "session", id, "from", sourceID, "cwd", cwd)
	m.hub.Emit(s.id, events.NewUserPrompt(text))
	s.Submit(text)
	return s.id, nil
}

// Lookup returns an in-memory session (nil if this server never ran it).
func (m *Manager) Lookup(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// StatusOf is the in-memory state, or "idle" for sessions not run here.
func (m *Manager) StatusOf(id string) string {
	if s := m.Lookup(id); s != nil {
		return s.State()
	}
	return "idle"
}

// Shutdown closes every warm process. No process starts after it began.
func (m *Manager) Shutdown() {
	m.closed.Store(true)
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range all {
		wg.Add(1)
		go func() { defer wg.Done(); defer guard.Recover(m.log, "session close"); s.close() }()
	}
	wg.Wait()
}

// ── permission option labels ───────────────────────────────

type suggestion struct {
	Type        string   `json:"type"`
	Mode        *string  `json:"mode"`
	Destination string   `json:"destination"`
	Directories []string `json:"directories"`
	Behavior    *string  `json:"behavior"`
	Rules       []struct {
		ToolName    any `json:"toolName"`
		RuleContent any `json:"ruleContent"`
	} `json:"rules"`
}

func describeDestination(d string) string {
	switch d {
	case "session":
		return "for this session"
	case "projectSettings":
		return "for this project"
	case "localSettings":
		return "in local settings"
	case "userSettings":
		return "in user settings"
	case "cliArg":
		return "from CLI settings"
	}
	return "for this project"
}

func shortDestination(d string) string {
	switch d {
	case "session":
		return "session"
	case "localSettings":
		return "local"
	case "userSettings":
		return "user"
	case "cliArg":
		return "CLI"
	}
	return "project"
}

func permissionOptions(sugg json.RawMessage, desc string, short bool) []events.Option {
	opts := []events.Option{{Text: "Yes", Key: "allow"}}
	if t := describeSuggestions(sugg, desc, short); t != "" {
		opts = append(opts, events.Option{Text: t, Key: "allowAlways"})
	}
	return append(opts, events.Option{Text: "No", Key: "deny"})
}

func describeSuggestions(sugg json.RawMessage, desc string, short bool) string {
	var list []json.RawMessage
	if json.Unmarshal(sugg, &list) != nil || len(list) == 0 {
		return ""
	}
	var first suggestion
	_ = json.Unmarshal(list[0], &first)
	dest := shortDestination(first.Destination)
	switch {
	case first.Type == "addDirectories" && len(first.Directories) > 0:
		if short {
			return "Always (" + dest + ")"
		}
		q := make([]string, len(first.Directories))
		for i, d := range first.Directories {
			q[i] = "`" + d + "`"
		}
		return "Yes, and always allow access to " + strings.Join(q, ", ") + " " + describeDestination(first.Destination)
	case (first.Type == "addRules" || first.Type == "replaceRules") && len(first.Rules) > 0:
		if short {
			return "Always (" + dest + ")"
		}
		r := first.Rules[0]
		tool := "this tool"
		if r.ToolName != nil {
			tool = jsString(r.ToolName)
		}
		content := ""
		if rc, ok := r.RuleContent.(string); ok && strings.TrimSpace(rc) != "" {
			content = " rule `" + strings.TrimSpace(rc) + "`"
		}
		behavior := "allow"
		if first.Behavior != nil {
			behavior = *first.Behavior
		}
		return "Yes, and always " + behavior + " " + tool + content + " " + describeDestination(first.Destination)
	case first.Type == "setMode" && first.Mode != nil:
		if short {
			if *first.Mode == "acceptEdits" {
				return "Accept edits (" + dest + ")"
			}
			return "Mode " + *first.Mode + " (" + dest + ")"
		}
		return "Yes, and use `" + *first.Mode + "` permission mode " + describeDestination(first.Destination)
	}
	if short {
		return "Always (" + dest + ")"
	}
	return desc
}

// SessionHealth is one live session in /api/ecc/health.
type SessionHealth struct {
	ID              string `json:"id"`
	State           string `json:"state"`
	Cwd             string `json:"cwd"`
	Pid             int    `json:"pid,omitempty"` // warm claude process; 0 = none
	Launching       bool   `json:"launching,omitempty"`
	Queued          int    `json:"queued,omitempty"`
	Permissions     int    `json:"permissions,omitempty"`
	Questions       int    `json:"questions,omitempty"`
	BackgroundTasks int    `json:"backgroundTasks,omitempty"`
	Agents          int    `json:"agents,omitempty"`
	Clients         int    `json:"clients"`
}

// Health lists the sessions this server has run, with their WebSocket clients.
func (m *Manager) Health() []SessionHealth {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	clients := m.hub.ClientCounts()
	out := make([]SessionHealth, 0, len(all))
	for _, s := range all {
		h := s.health()
		h.Clients = clients[h.ID]
		out = append(out, h)
	}
	return out
}
