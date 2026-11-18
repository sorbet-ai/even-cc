package claude

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"even-cc/internal/events"
	"even-cc/internal/guard"
	"even-cc/internal/jsonx"
)

// procHandle is the part of *Proc a Session uses (faked in tests).
type procHandle interface {
	Send(v any)
	StartRequest(subtype string, fields map[string]any) func(time.Duration) (json.RawMessage, error)
	Respond(requestID string, response any)
	RespondError(requestID, msg string)
	Close()
	Pid() int
	ExitDescription() string
	ExitCode() int
}

type pendingPerm struct {
	proc        procHandle
	reqID       string
	toolName    string
	toolUseID   string
	input       json.RawMessage
	suggestions json.RawMessage
	description string
	offered     map[string]bool
	todoWrite   bool
	timer       *time.Timer
	// blocksTurn: the main agent waits on this request (it arrived during a
	// turn and isn't a subagent's). Requests still open at a result belong to
	// subagents and stay open until answered, timed out or the process ends.
	blocksTurn bool
}

type pendingQuestion struct {
	proc         procHandle
	reqID        string
	toolUseID    string
	questions    []events.Question
	rawQuestions json.RawMessage
	timer        *time.Timer
	blocksTurn   bool
}

// Session is one Claude Code conversation driven by a warm process.
type Session struct {
	m   *Manager
	log *slog.Logger

	mu  sync.Mutex
	id  string
	cwd string
	// forkFrom: the session this one forks on its first launch (set before
	// the session is shared, then read-only).
	forkFrom string

	proc      procHandle
	gen       int
	launching bool
	usesState bool
	// cliState is the CLI's last session_state_changed state (running |
	// idle; "" before the first). The CLI stays running after a result while
	// a background agent works.
	cliState string

	busy        bool
	busyEmitted bool
	turnSeq     int
	turnStart   time.Time
	// sentSinceResult: a prompt was written to the CLI mid-turn after the
	// last result (an idle may cross it).
	sentSinceResult bool
	resultSent      bool // a result was emitted in this turn
	// replyStart: start of the reply the next footer describes (turn start,
	// or the first main-agent frame after a result in the same CLI turn).
	replyStart    time.Time
	resultPending bool
	interrupted   bool
	lastFailed    bool
	queue         []string

	perms     []*pendingPerm
	questions []*pendingQuestion

	mapper       *Mapper
	lastSeenUUID string
	recycleNext  bool
	bgTasks      map[string]bool
	idleTimer    *time.Timer

	permMode  string
	model     string
	lastModel string

	costBase  float64
	usageBase map[string]modelUsage

	agentTicker bool // a goroutine refreshes subagent lines

	// Talk while busy (talk.go).
	talkIdle  bool            // status idle sent mid-turn; the app thinks the turn ended
	talkTimer *time.Timer     // armed while a tool runs with nothing else going on
	talkTools map[string]bool // main-agent tools executing (tool_use seen, no tool_result)
	talkUsed  map[string]bool // tools that already had their idle/busy pair
}

func newSession(m *Manager, id, cwd string) *Session {
	s := &Session{
		m:        m,
		log:      m.log.With("session", id),
		id:       id,
		cwd:      cwd,
		bgTasks:  map[string]bool{},
		permMode: m.opts.PermissionMode,
		model:    m.opts.Model,
	}
	s.mapper = NewMapper(s.emit, s.emitStatus, m.Display)
	return s
}

// emit sends an event to the app (s.mu held). While the app was told idle
// mid-turn (talk while busy), busy is re-sent first.
func (s *Session) emit(ev any) {
	s.talkGateLocked(ev)
	s.m.hub.Emit(s.id, ev)
}
func (s *Session) emitStatus(state string)  { s.emit(events.NewStatus(state, s.id)) }
func (s *Session) notify(title, msg string) { s.emit(events.NewNotification(title, msg)) }

// State is idle | busy | awaiting.
func (s *Session) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

func (s *Session) stateLocked() string {
	if len(s.perms) > 0 || len(s.questions) > 0 {
		return "awaiting"
	}
	if s.busy && !s.talkIdle { // what the app was told
		return "busy"
	}
	return "idle"
}

// Submit runs text now. While a turn runs it is written straight to the
// CLI, which folds it into that turn (verified with 2.1.312: a message sent
// during a tool call is answered in the same result; one sent after a result
// while a background agent keeps the CLI running is answered at once). It is
// queued only while a process is launching.
func (s *Session) Submit(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.answerPendingWithPromptLocked(text) {
		return
	}
	if !s.busy {
		s.beginTurnLocked(text)
		return
	}
	if isStopWord(text) {
		// Spoken "stop" while a turn runs (the app showed idle during a long
		// tool): stop the turn instead of telling Claude.
		s.log.Info("stop word mid-turn, interrupting", "text", text, "appIdle", s.talkIdle)
		s.talkWakeLocked("stop word")
		s.interruptLocked()
		return
	}
	if s.proc == nil || s.launching {
		s.queue = append(s.queue, text)
		s.log.Info("prompt queued", "queue", len(s.queue))
		return
	}
	s.log.Info("prompt sent mid-turn", "pid", s.proc.Pid(), "appIdle", s.talkIdle, "text", truncEllipsis(text, 80))
	// The app showed idle so the user could talk; it sent this prompt and
	// now gets busy as after any prompt (user_prompt → busy).
	s.talkWakeLocked("prompt")
	defer s.talkUpdateLocked()
	s.resultPending = false // more output is coming
	s.sentSinceResult = true
	if s.resultSent {
		// The reply to this prompt starts with busy, as for a queued
		// prompt in even-terminal (result → busy → …).
		s.resultSent = false
		s.busyEmitted = false
	}
	if s.replyStart.IsZero() {
		s.replyStart = time.Now()
	}
	s.sendUserLocked(text)
}

// answerPendingWithPromptLocked: while a question or permission prompt is
// pending, the CLI is blocked inside can_use_tool, so a prompt written to it
// would sit unread until that request is answered. A prompt sent then (the
// app may not show the question, or the user just talks) answers it instead:
// a question gets the text as a free-text ("Other") answer to each question;
// a permission is denied with the text as the message Claude sees. A stop
// word is left to the interrupt path (which resolves both).
//
// Only requests the main agent is blocked on count: a subagent's request
// doesn't stop the CLI from reading a prompt.
func (s *Session) answerPendingWithPromptLocked(text string) bool {
	if isStopWord(text) {
		return false
	}
	for _, pq := range s.questions {
		if pq.blocksTurn {
			s.removeQuestionLocked(pq)
			s.log.Info("prompt answers the pending question", "text", truncEllipsis(text, 80))
			s.finishQuestionWithLocked(pq, textAnswers(text, pq.questions), true)
			s.talkUpdateLocked()
			return true
		}
	}
	for _, pp := range s.perms {
		if pp.blocksTurn {
			s.removePermLocked(pp)
			s.log.Info("prompt denies the pending permission request", "tool", pp.toolName, "text", truncEllipsis(text, 80))
			s.finishPermMessageLocked(pp, "deny", "The user didn't answer this permission request and said instead: "+text, true)
			s.talkUpdateLocked()
			return true
		}
	}
	return false
}

func (s *Session) resetTurnLocked() {
	s.turnSeq++
	s.busy = true
	s.busyEmitted = false
	s.resultPending = false
	s.interrupted = false
	s.lastFailed = false
	s.turnStart = time.Now()
	s.replyStart = s.turnStart
	s.sentSinceResult = false
	s.resultSent = false
	s.mapper.ResetTurn()
	s.talkResetLocked()
	s.clearIdleTimerLocked()
	s.startStatsLocked()
}

func (s *Session) beginTurnLocked(text string) {
	s.resetTurnLocked()
	if s.proc != nil && !s.launching {
		if !s.staleLocked() {
			s.log.Info("reusing warm process", "pid", s.proc.Pid())
			// Written after a result while the CLI still runs (it goes idle
			// moments after a result, or only when a background agent ends):
			// its idle may cross this prompt.
			s.sentSinceResult = s.usesState && s.cliState == "running"
			s.sendUserLocked(text)
			return
		}
		old := s.proc
		s.gen++
		s.proc = nil
		s.bgTasks = map[string]bool{}
		s.mapper.StopAgents(false)
		s.dropPendingLocked("process replaced")
		s.launching = true
		go s.launchThenSend(old, text)
		return
	}
	s.launching = true
	go s.launchThenSend(nil, text)
}

func (s *Session) sendUserLocked(text string) {
	s.proc.Send(struct {
		Type            string `json:"type"`
		Message         any    `json:"message"`
		ParentToolUseID any    `json:"parent_tool_use_id"`
	}{"user", map[string]string{"role": "user", "content": text}, nil})
}

// staleLocked decides whether the warm process must be replaced before the
// next prompt: the last turn failed, or something else wrote to the
// transcript since our last turn (unless background tasks are running).
func (s *Session) staleLocked() bool {
	if s.recycleNext {
		s.recycleNext = false
		s.log.Info("recycling process after failed turn")
		return true
	}
	cur, err := s.m.store.LastUUID(s.id)
	if err != nil {
		s.log.Warn("transcript check failed, restarting", "err", err)
		return true
	}
	if cur != s.lastSeenUUID {
		if len(s.bgTasks) > 0 {
			s.log.Info("transcript changed elsewhere, keeping process for background tasks", "tasks", len(s.bgTasks))
			return false
		}
		s.log.Info("transcript changed elsewhere, restarting process", "was", s.lastSeenUUID, "now", cur)
		return true
	}
	return false
}

func (s *Session) launchThenSend(old procHandle, text string) {
	defer guard.Recover(s.log, "process launch")
	if old != nil {
		old.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.launching = false
	if s.interrupted {
		// Stopped (stop word, /api/interrupt) while the process was being
		// replaced: nothing reached Claude, so nothing runs.
		s.log.Info("interrupted during launch; prompt not sent", "dropped", len(s.queue))
		s.queue = nil
		s.emit(events.Result{Type: "result", Success: false, Text: "Interrupted by user", SessionID: s.id, Provider: events.Provider})
		s.mapper.FlushAll()
		s.finishTurnLocked()
		return
	}
	if err := s.startProcLocked(); err != nil {
		s.log.Error("failed to start claude", "err", err)
		s.emit(events.NewError(err.Error()))
		s.mapper.FlushAll()
		s.finishTurnLocked()
		return
	}
	s.sendUserLocked(text)
	for _, q := range s.queue { // prompts that arrived during the launch
		s.sendUserLocked(q)
	}
	s.queue = nil
}

func (s *Session) startProcLocked() error {
	if s.m.closed.Load() {
		return errors.New("even-cc is shutting down")
	}
	if st, err := os.Stat(s.cwd); err != nil || !st.IsDir() {
		return &os.PathError{Op: "session cwd", Path: s.cwd, Err: os.ErrNotExist}
	}
	spec := s.m.procSpec(s)
	gen := s.gen
	p, err := s.m.launch(spec, func(line []byte) { s.onLine(gen, line) }, func(p procHandle) { s.onExit(gen, p) })
	if err != nil {
		return err
	}
	s.log.Info("claude started", "pid", p.Pid(), "resume", hasArg(spec.Args, "--resume"), "cwd", s.cwd)
	s.proc = p
	s.usesState = false
	s.cliState = ""
	s.costBase = 0
	s.usageBase = nil
	init := map[string]any{
		"hooks": map[string]any{"Notification": []any{map[string]any{"hookCallbackIds": []string{"ecc_notification"}}}},
		// Without it the CLI's interrupt also kills every background agent
		// and shell (2.1.312); with it, interrupt only aborts the turn.
		"perTaskStopAffordance": true,
	}
	if s.m.Display().SubagentProgress {
		// task_progress then carries a model-written one-line summary (~30 s).
		init["agentProgressSummaries"] = true
	}
	wait := p.StartRequest("initialize", init)
	go func() {
		defer guard.Recover(s.log, "initialize")
		resp, err := wait(30 * time.Second)
		if err != nil {
			s.log.Warn("initialize failed", "err", err)
			return
		}
		if s.m.onInitialize != nil {
			s.m.onInitialize(resp)
		}
	}()
	return nil
}

func hasArg(args []string, a string) bool {
	for _, x := range args {
		if x == a {
			return true
		}
	}
	return false
}

func (s *Session) startStatsLocked() {
	seq := s.turnSeq
	interval := s.m.opts.StatsInterval
	if interval <= 0 {
		return
	}
	go func() {
		defer guard.Recover(s.log, "running_stats ticker")
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			s.mu.Lock()
			if !s.busy || s.turnSeq != seq {
				s.mu.Unlock()
				return
			}
			if s.resultSent || s.talkIdle { // never after the result, nor while the app was told idle
				s.mu.Unlock()
				continue
			}
			in, out := s.mapper.Stats()
			s.emit(events.RunningStats{Type: "running_stats", DurationMs: time.Since(s.turnStart).Milliseconds(), InputTokens: in, OutputTokens: out})
			s.mu.Unlock()
		}
	}()
}

// ensureAgentTickerLocked keeps subagent lines' elapsed time fresh while any
// subagent runs (including background ones after the turn ended).
func (s *Session) ensureAgentTickerLocked() {
	tick := s.m.opts.AgentTick
	if s.agentTicker || tick <= 0 {
		return
	}
	s.agentTicker = true
	gen := s.gen
	go func() {
		defer guard.Recover(s.log, "subagent ticker")
		t := time.NewTicker(tick)
		defer t.Stop()
		for range t.C {
			s.mu.Lock()
			if s.gen != gen || s.mapper.RunningAgents() == 0 {
				s.agentTicker = false
				s.mu.Unlock()
				return
			}
			s.mapper.TickAgents()
			s.mu.Unlock()
		}
	}()
}

func (s *Session) clearIdleTimerLocked() {
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

func (s *Session) armIdleTimerLocked() {
	s.clearIdleTimerLocked()
	d := s.m.opts.IdleTimeout
	if d <= 0 || len(s.bgTasks) > 0 || s.proc == nil {
		return
	}
	s.idleTimer = time.AfterFunc(d, func() {
		defer guard.Recover(s.log, "idle timeout")
		s.mu.Lock()
		if s.busy || len(s.bgTasks) > 0 || s.proc == nil || s.launching {
			s.mu.Unlock()
			return
		}
		old := s.proc
		s.gen++
		s.proc = nil
		s.dropPendingLocked("idle timeout")
		s.mu.Unlock()
		s.log.Info("idle timeout, closing warm process")
		old.Close()
	})
}

// finishTurnLocked ends the current turn: next queued prompt or idle.
func (s *Session) finishTurnLocked() {
	s.busy = false
	s.turnSeq++
	s.resultPending = false
	s.talkResetLocked()
	s.keepPendingForSubagentsLocked()
	s.mapper.SetActive(false)
	if s.proc != nil {
		if s.lastFailed {
			s.recycleNext = true
		}
		if uuid, err := s.m.store.LastUUID(s.id); err != nil {
			s.log.Warn("transcript read failed", "err", err)
			s.recycleNext = true
		} else {
			s.lastSeenUUID = uuid
		}
	}
	if len(s.queue) > 0 {
		next := s.queue[0]
		s.queue = s.queue[1:]
		s.log.Info("dispatching queued prompt", "remaining", len(s.queue))
		s.beginTurnLocked(next)
		return
	}
	s.emitStatus("idle")
	s.armIdleTimerLocked()
}

func (s *Session) endTurnLocked() {
	s.mapper.FlushAll()
	s.finishTurnLocked()
}

// startAutonomousLocked tracks a turn Claude started on its own (e.g. a
// background task finished) like a prompt.
func (s *Session) startAutonomousLocked() {
	s.log.Info("autonomous turn started")
	s.resetTurnLocked()
}

func (s *Session) ensureBusyLocked() {
	if s.busy && !s.busyEmitted {
		s.busyEmitted = true
		s.emitStatus("busy")
		s.mapper.SetActive(true)
		s.mapper.FlushHeld() // background-agent lines withheld while idle
	}
}

type cliMsg struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	SessionID       string          `json:"session_id"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	State           string          `json:"state"`
	RequestID       string          `json:"request_id"`
	Request         json.RawMessage `json:"request"`
	Event           json.RawMessage `json:"event"`
	Message         json.RawMessage `json:"message"`
	IsReplay        bool            `json:"isReplay"`
	Model           string          `json:"model"`
	PermissionMode  string          `json:"permissionMode"`
	TaskID          string          `json:"task_id"`
	Tasks           []struct {
		TaskID string `json:"task_id"`
	} `json:"tasks"`
	Attempt     int             `json:"attempt"`
	MaxRetries  int             `json:"max_retries"`
	ErrorStatus json.RawMessage `json:"error_status"`
}

func (s *Session) onLine(gen int, line []byte) {
	defer guard.Recover(s.log, "cli line handler")
	var msg cliMsg
	if err := decodeLenient(s.log, line, &msg); err != nil {
		s.log.Warn("unparseable cli line dropped", "err", err, "line", truncEllipsis(string(line), 300))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen || s.proc == nil {
		return // output of a process we already replaced, or that exited
	}
	defer s.talkUpdateLocked()
	p := s.proc
	switch msg.Type {
	case "control_request":
		s.handleControlRequestLocked(p, msg)
		return
	case "control_cancel_request":
		s.cancelRequestLocked(msg.RequestID)
		return
	case "keep_alive":
		return
	}

	if msg.Type == "system" && msg.Subtype == "session_state_changed" {
		s.usesState = true
		s.cliState = msg.State
		switch msg.State {
		case "running":
			if !s.busy {
				s.startAutonomousLocked()
			}
			s.resultPending = false
			s.ensureBusyLocked()
		case "idle":
			if !s.busy {
				break
			}
			if s.sentSinceResult {
				// A prompt we wrote after the last result may have crossed
				// this idle; the CLI then goes running again for it. Safety
				// net: end the turn if nothing follows.
				s.armResultGraceLocked("no output after prompt that crossed an idle; ending turn")
				break
			}
			s.endTurnLocked()
		}
		return
	}

	parentID := ""
	if msg.ParentToolUseID != nil {
		parentID = *msg.ParentToolUseID
	}
	// Only main-thread frames say anything about the turn. A background
	// subagent keeps streaming (parent_tool_use_id set) after the turn's
	// result, and the CLI then stays "running" with no idle until it ends.
	main := parentID == ""
	if main && s.talkIdle {
		switch msg.Type {
		case "stream_event", "assistant", "result":
			s.talkWakeLocked(msg.Type)
		case "user":
			if !msg.IsReplay {
				s.talkWakeLocked("tool result")
			}
		}
	}
	switch msg.Type {
	case "stream_event", "assistant":
		if main {
			// Main-agent output while idle: Claude started a turn on its own
			// (no session_state frames), or continues a CLI turn whose
			// result already ended ours.
			if !s.busy {
				s.startAutonomousLocked()
			}
			if s.replyStart.IsZero() {
				s.replyStart = time.Now()
			}
			s.resultPending = false
		}
	case "user":
		if main {
			s.resultPending = false
		}
	}
	if msg.Type != "rate_limit_event" {
		s.ensureBusyLocked()
	} else {
		s.m.noteRateLimit(line)
	}

	switch msg.Type {
	case "system":
		s.handleSystemLocked(msg, line)
	case "stream_event":
		if parentID == "" {
			s.mapper.StreamEvent(msg.Event)
		}
	case "assistant":
		var m struct {
			Model   string         `json:"model"`
			Content []contentBlock `json:"content"`
		}
		if json.Unmarshal(msg.Message, &m) == nil {
			if parentID == "" {
				s.mapper.SetMainModel(m.Model)
				s.talkToolsStartedLocked(m.Content)
			}
			s.mapper.Assistant(m.Content, parentID)
		}
	case "user":
		if msg.IsReplay {
			return
		}
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(msg.Message, &m) != nil || len(m.Content) == 0 || m.Content[0] != '[' {
			return
		}
		var blocks []contentBlock
		if json.Unmarshal(m.Content, &blocks) == nil {
			if parentID == "" {
				s.talkToolsEndedLocked(blocks)
			}
			s.mapper.ToolResults(blocks)
		}
	case "result":
		s.handleResultLocked(line)
	}
}

func (s *Session) handleSystemLocked(msg cliMsg, line []byte) {
	switch msg.Subtype {
	case "task_started", "task_progress", "task_updated", "task_notification":
		if s.mapper.Task(line) {
			s.ensureAgentTickerLocked()
		}
	}
	switch msg.Subtype {
	case "init":
		if msg.Model != "" {
			s.lastModel = msg.Model
		}
		if msg.PermissionMode != "" {
			s.permMode = msg.PermissionMode
		}
	case "status":
		if msg.PermissionMode != "" {
			s.permMode = msg.PermissionMode
		}
	case "task_started":
		if msg.TaskID != "" {
			s.bgTasks[msg.TaskID] = true
			s.log.Info("background task started", "task", msg.TaskID, "running", len(s.bgTasks))
		}
	case "task_notification":
		if msg.TaskID != "" {
			delete(s.bgTasks, msg.TaskID)
			s.log.Info("background task finished", "task", msg.TaskID, "running", len(s.bgTasks))
			if len(s.bgTasks) == 0 && !s.busy {
				s.armIdleTimerLocked()
			}
		}
	case "background_tasks_changed":
		set := map[string]bool{}
		for _, t := range msg.Tasks {
			if t.TaskID != "" {
				set[t.TaskID] = true
			}
		}
		s.bgTasks = set
		if len(set) == 0 && !s.busy {
			s.armIdleTimerLocked()
		}
	case "api_retry":
		status := ""
		if len(msg.ErrorStatus) > 0 && string(msg.ErrorStatus) != "null" {
			status = ", HTTP " + strings.Trim(string(msg.ErrorStatus), `"`)
		}
		s.notify("API Retry", "Retrying ("+strconv.Itoa(msg.Attempt)+"/"+strconv.Itoa(msg.MaxRetries)+")"+status+"...")
	}
}

type modelUsage struct {
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CostUSD      float64 `json:"costUSD"`
	// ContextWindow is the model's window size (tokens).
	ContextWindow int64 `json:"contextWindow"`
}

type cliResult struct {
	Subtype        string                `json:"subtype"`
	Result         *string               `json:"result"`
	SessionID      string                `json:"session_id"`
	TotalCost      *float64              `json:"total_cost_usd"`
	NumTurns       int64                 `json:"num_turns"`
	DurationMs     int64                 `json:"duration_ms"`
	Errors         json.RawMessage       `json:"errors"`
	TerminalReason string                `json:"terminal_reason"`
	ModelUsage     map[string]modelUsage `json:"modelUsage"`
	Since          map[string]modelUsage `json:"model_usage_since_last_result"`
}

func (s *Session) handleResultLocked(line []byte) {
	var r cliResult
	if err := decodeLenient(s.log, line, &r); err != nil {
		// Still a result: the turn must end, or the session stays busy.
		s.log.Warn("unreadable result; ending the turn", "err", err)
		r = cliResult{Subtype: "error_during_execution"}
	}
	s.sentSinceResult = false
	s.mapper.FlushAll()
	windows := map[string]int64{}
	for k, u := range r.ModelUsage {
		if u.ContextWindow > 0 {
			windows[k] = u.ContextWindow
		}
	}
	s.mapper.SetContextWindows(windows)
	ev, interrupted := s.buildResultLocked(r)
	if r.Subtype != "success" && !interrupted {
		s.lastFailed = true
		s.log.Warn("turn failed", "subtype", r.Subtype, "reason", r.TerminalReason, "errors", strings.Join(errorTexts(r.Errors), "; "))
	}
	// A leftover background-task notification can yield an empty zero-turn
	// result right before the real turn (even-protocol §5.2 quirk 1). Drop it.
	stray := r.Subtype == "success" && r.NumTurns == 0 && (r.Result == nil || *r.Result == "")
	if stray {
		s.log.Info("dropped empty zero-turn result")
		return
	}
	if !s.busy {
		// A result with no main-agent output before it (Claude reported on a
		// background task straight away): still busy … result, idle.
		s.startAutonomousLocked()
		s.ensureBusyLocked()
	}

	// Footer only on successful turns: interrupted ones already read
	// "Interrupted by user", failed ones carry the error text.
	if ev.Success && s.m.Display().Footer {
		start := s.replyStart
		if start.IsZero() {
			start = s.turnStart
		}
		footer := s.mapper.Footer(time.Since(start))
		s.emit(events.TextDelta{Type: "text_delta", Text: "\n\n" + footer})
		if ev.Text == "" {
			ev.Text = footer
		} else {
			ev.Text += "\n\n" + footer
		}
	}
	s.log.Info("turn result", "success", ev.Success, "turns", ev.Turns, "cost", ev.CostUsd, "ms", ev.DurationMs)
	s.emit(ev)
	// The CLI can continue the same turn after a result (it stays
	// "running" while a background agent works, then reports): the next
	// footer covers only what follows.
	s.replyStart = time.Time{}
	s.mapper.ResetFooter()
	s.resultSent = true
	// The turn is over for the app now. The CLI sends its idle moments
	// later, or, while a background agent runs, only after the agent ended
	// and Claude reported on it; any main-agent output after this result
	// is a new (autonomous) turn with its own busy … result, idle.
	s.endTurnLocked()
}

// armResultGraceLocked ends the turn after ResultIdleGrace unless the main
// agent produces output (or another result/idle settles it) meanwhile.
func (s *Session) armResultGraceLocked(why string) {
	s.resultPending = true
	seq := s.turnSeq
	time.AfterFunc(s.m.opts.ResultIdleGrace, func() {
		defer guard.Recover(s.log, "result grace timer")
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.busy && s.turnSeq == seq && s.resultPending {
			s.log.Info(why)
			s.endTurnLocked()
		}
	})
}

func (s *Session) buildResultLocked(r cliResult) (events.Result, bool) {
	interrupted := r.Subtype == "error_during_execution" &&
		(strings.HasPrefix(r.TerminalReason, "aborted") || s.interrupted)
	text := ""
	if r.Result != nil {
		text = *r.Result
	}
	if r.Subtype != "success" {
		errs := strings.Join(errorTexts(r.Errors), "\n")
		switch {
		case interrupted:
			text = "Interrupted by user"
		case r.Subtype == "error_max_turns":
			text = errs
			if text == "" {
				text = "Reached max turns limit (" + strconv.FormatInt(r.NumTurns, 10) + " turns). Try breaking the task into smaller steps."
			}
		case r.Subtype == "error_max_budget_usd":
			text = errs
			if text == "" {
				text = "Session budget exhausted."
			}
		default:
			text = errs
		}
	}
	// Per-turn numbers: the CLI's totals are cumulative per process.
	var cost float64
	var in, out int64
	if r.Since != nil {
		for _, u := range r.Since {
			cost += u.CostUSD
			in += u.InputTokens
			out += u.OutputTokens
		}
	} else {
		total := 0.0
		if r.TotalCost != nil {
			total = *r.TotalCost
		}
		cost = total - s.costBase
		for k, u := range r.ModelUsage {
			b := s.usageBase[k]
			in += u.InputTokens - b.InputTokens
			out += u.OutputTokens - b.OutputTokens
		}
	}
	if r.TotalCost != nil {
		s.costBase = *r.TotalCost
	}
	if r.ModelUsage != nil {
		s.usageBase = r.ModelUsage
	}
	if cost < 0 {
		cost = 0
	}
	sid := r.SessionID
	if sid == "" {
		sid = s.id
	}
	return events.Result{
		Type: "result", Success: r.Subtype == "success", Text: text, SessionID: sid,
		CostUsd: cost, Provider: events.Provider, Turns: r.NumTurns, DurationMs: r.DurationMs,
		InputTokens: in, OutputTokens: out,
	}, interrupted
}

func (s *Session) onExit(gen int, p procHandle) {
	defer guard.Recover(s.log, "cli exit handler")
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen || s.proc != p {
		return
	}
	code := p.ExitCode()
	s.proc = nil
	s.bgTasks = map[string]bool{}
	s.mapper.StopAgents(false)
	s.clearIdleTimerLocked()
	s.resolveAllPendingLocked(false)
	if !s.busy {
		return
	}
	switch {
	case s.interrupted || s.resultPending:
	case code != 0 && code != 143:
		s.emit(events.NewError(p.ExitDescription()))
	case code == 0 && !s.resultSent:
		s.log.Warn("claude exited mid-turn without a result", "code", code)
		s.emit(events.NewError("Claude Code exited before replying (code " + strconv.Itoa(code) + ")"))
	}
	s.recycleNext = false
	s.lastFailed = false
	s.mapper.FlushAll()
	s.finishTurnLocked()
}

// ── control requests from the CLI ─────────────────────────────

func (s *Session) handleControlRequestLocked(p procHandle, msg cliMsg) {
	if p == nil {
		return
	}
	var req struct {
		Subtype     string          `json:"subtype"`
		ToolName    string          `json:"tool_name"`
		Input       json.RawMessage `json:"input"`
		ToolUseID   string          `json:"tool_use_id"`
		Suggestions json.RawMessage `json:"permission_suggestions"`
		CallbackID  string          `json:"callback_id"`
		AgentID     string          `json:"agent_id"`
	}
	if err := json.Unmarshal(msg.Request, &req); err != nil {
		s.log.Warn("control request partly unreadable", "err", err, "subtype", req.Subtype)
	}
	switch req.Subtype {
	case "can_use_tool":
		// While shown idle (talk while busy), a prompt that reaches the app
		// gets busy first (emit gate); auto-allowed requests stay silent.
		s.ensureBusyLocked()
		s.handleCanUseToolLocked(p, msg.RequestID, req.ToolName, req.Input, req.ToolUseID, req.Suggestions, s.busy && req.AgentID == "")
	case "hook_callback":
		if req.CallbackID == "ecc_notification" {
			var hin struct {
				Title   string `json:"title"`
				Message string `json:"message"`
				Type    string `json:"notification_type"`
			}
			_ = json.Unmarshal(req.Input, &hin)
			title := hin.Title
			if title == "" {
				title = "Notice"
			}
			if redundantNotice(hin.Type, hin.Message) {
				s.log.Debug("notification dropped (the prompt itself is shown)", "message", hin.Message)
			} else {
				s.notify(title, hin.Message)
			}
		}
		p.Respond(msg.RequestID, struct{}{})
	case "request_user_dialog":
		// The SDK stays silent for dialog kinds it didn't declare.
	default:
		p.RespondError(msg.RequestID, "Unsupported control request subtype: "+req.Subtype)
	}
}

func (s *Session) handleCanUseToolLocked(p procHandle, reqID, tool string, input json.RawMessage, toolUseID string, suggestions json.RawMessage, blocksTurn bool) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	in := parseInput(input)
	allow := func() { p.Respond(reqID, allowResponse(input, toolUseID, nil)) }
	auto := s.m.autoAllowed
	switch {
	case tool == "AskUserQuestion":
		s.askQuestionLocked(p, reqID, input, toolUseID, blocksTurn)
		return
	case tool == "Bash" && isAutoAllowedBash(in["command"]):
		allow()
		return
	case tool == "TodoWrite" && auto[tool]:
		s.emitTodoProgressLocked(in)
		allow()
		return
	case tool == "TaskUpdate" && auto[tool]:
		allow()
		return
	case auto[tool] && tool != "TodoWrite" && tool != "TaskUpdate":
		allow()
		return
	}
	d := s.m.Display()
	desc := ClassicSummary(tool, in)
	if d.Rich {
		desc = RichSummary(tool, in, nil)
	}
	sugg := suggestions
	if len(sugg) == 0 || string(sugg) == "null" {
		sugg = json.RawMessage("null")
	}
	opts := permissionOptions(sugg, desc, d.ShortOptions)
	offered := map[string]bool{}
	for _, o := range opts {
		offered[o.Key] = true
	}
	pp := &pendingPerm{proc: p, reqID: reqID, toolName: tool, toolUseID: toolUseID, input: input,
		suggestions: sugg, description: desc, offered: offered, todoWrite: tool == "TodoWrite", blocksTurn: blocksTurn}
	s.perms = append(s.perms, pp)
	s.log.Info("permission request", "tool", tool, "desc", desc)
	s.emit(events.PermissionRequest{Type: "permission_request", ToolName: tool, Description: desc,
		Detail: detailFromInput(in), ToolUseID: toolUseID, Options: opts, Suggestions: sugg})
	pp.timer = time.AfterFunc(s.m.opts.PermissionTimeout, func() {
		defer guard.Recover(s.log, "permission timeout")
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.removePermLocked(pp) {
			s.log.Info("permission timed out", "tool", tool)
			s.finishPermLocked(pp, "deny", true)
			s.talkUpdateLocked()
		}
	})
}

func allowResponse(input json.RawMessage, toolUseID string, perms json.RawMessage) map[string]any {
	r := map[string]any{"behavior": "allow", "updatedInput": input, "toolUseID": toolUseID}
	if len(perms) > 0 && string(perms) != "null" {
		r["updatedPermissions"] = perms
	}
	return r
}

func (s *Session) emitTodoProgressLocked(in toolInput) {
	todos, _ := in["todos"].([]any)
	if len(todos) == 0 {
		return
	}
	completed := 0
	current := ""
	found := false
	for _, t := range todos {
		m, _ := t.(map[string]any)
		if m == nil {
			continue
		}
		if m["status"] == "completed" {
			completed++
		}
		if !found && m["status"] == "in_progress" {
			found = true
			if v, ok := m["content"]; ok && v != nil {
				current = jsString(v)
			} else {
				current = jsString(m["activeForm"])
			}
		}
	}
	if !found && completed == len(todos) {
		current = "All done"
	}
	s.emit(events.TaskProgress{Type: "task_progress", Completed: completed, Total: len(todos), Current: current})
}

func (s *Session) removePermLocked(pp *pendingPerm) bool {
	for i, x := range s.perms {
		if x == pp {
			s.perms = append(s.perms[:i], s.perms[i+1:]...)
			return true
		}
	}
	return false
}

// finishPermLocked emits permission_result and answers the CLI.
func (s *Session) finishPermLocked(pp *pendingPerm, decision string, reply bool) {
	s.finishPermMessageLocked(pp, decision, "Denied by user", reply)
}

// finishPermMessageLocked is finishPermLocked with the deny message Claude
// gets as the tool result.
func (s *Session) finishPermMessageLocked(pp *pendingPerm, decision, denyMsg string, reply bool) {
	if pp.timer != nil {
		pp.timer.Stop()
	}
	result := "denied"
	switch decision {
	case "allow":
		result = "allowed"
	case "allowAlways":
		result = "always"
	}
	s.emit(events.PermissionResult{Type: "permission_result", ToolName: pp.toolName, Summary: pp.description, Decision: result})
	if result == "denied" {
		s.mapper.MarkDenied(pp.toolUseID)
	}
	if !reply {
		return
	}
	if result == "denied" {
		pp.proc.Respond(pp.reqID, map[string]any{"behavior": "deny", "message": denyMsg, "toolUseID": pp.toolUseID})
		return
	}
	if pp.todoWrite {
		s.emitTodoProgressLocked(parseInput(pp.input))
	}
	var perms json.RawMessage
	if result == "always" {
		perms = pp.suggestions
	}
	pp.proc.Respond(pp.reqID, allowResponse(pp.input, pp.toolUseID, perms))
}

// RespondPermission answers the oldest pending permission request. false if
// none is pending or the decision wasn't offered.
func (s *Session) RespondPermission(decision string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.perms) == 0 {
		return false
	}
	pp := s.perms[0]
	if !pp.offered[decision] {
		return false
	}
	s.perms = s.perms[1:]
	s.log.Info("permission answered", "tool", pp.toolName, "decision", decision)
	s.finishPermLocked(pp, decision, true)
	s.talkUpdateLocked()
	return true
}

func normalizeQuestions(raw json.RawMessage) []events.Question {
	var qs []map[string]any
	_ = json.Unmarshal(raw, &qs)
	out := make([]events.Question, 0, len(qs))
	str := func(m map[string]any, k string) string {
		if m == nil {
			return ""
		}
		return jsString(m[k])
	}
	for _, q := range qs {
		nq := events.Question{Question: str(q, "question"), Header: str(q, "header"), MultiSelect: q["multiSelect"] == true, Options: []events.QuestionOption{}}
		if opts, ok := q["options"].([]any); ok {
			for _, o := range opts {
				om, _ := o.(map[string]any)
				nq.Options = append(nq.Options, events.QuestionOption{Label: str(om, "label"), Description: str(om, "description"), Preview: str(om, "preview")})
			}
		}
		out = append(out, nq)
	}
	return out
}

func (s *Session) askQuestionLocked(p procHandle, reqID string, input json.RawMessage, toolUseID string, blocksTurn bool) {
	var in struct {
		Questions json.RawMessage `json:"questions"`
	}
	_ = json.Unmarshal(input, &in)
	if len(in.Questions) == 0 {
		in.Questions = json.RawMessage("null")
	}
	qs := normalizeQuestions(in.Questions)
	pq := &pendingQuestion{proc: p, reqID: reqID, toolUseID: toolUseID, questions: qs, rawQuestions: in.Questions, blocksTurn: blocksTurn}
	s.questions = append(s.questions, pq)
	s.log.Info("question", "count", len(qs))
	s.emit(events.UserQuestion{Type: "user_question", Questions: qs, ToolUseID: toolUseID})
	pq.timer = time.AfterFunc(s.m.opts.QuestionTimeout, func() {
		defer guard.Recover(s.log, "question timeout")
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.removeQuestionLocked(pq) {
			s.log.Info("question timed out")
			s.finishQuestionLocked(pq, "skip", true)
			s.talkUpdateLocked()
		}
	})
}

func (s *Session) removeQuestionLocked(pq *pendingQuestion) bool {
	for i, x := range s.questions {
		if x == pq {
			s.questions = append(s.questions[:i], s.questions[i+1:]...)
			return true
		}
	}
	return false
}

// answersFor encodes the app's answer string like even-terminal: valid JSON
// is used as-is, anything else is the answer to every question.
func answersFor(answer string, qs []events.Question) json.RawMessage {
	if json.Valid([]byte(answer)) {
		if v, err := jsonx.Parse([]byte(answer)); err == nil {
			b, _ := v.MarshalJSON()
			return b
		}
	}
	obj := &jsonx.Value{Kind: jsonx.Object, Obj: []jsonx.Member{}}
	for _, q := range qs {
		key := q.Question
		if key == "" {
			key = q.Header
		}
		replaced := false
		for i := range obj.Obj {
			if obj.Obj[i].Key == key {
				obj.Obj[i].Value = &jsonx.Value{Kind: jsonx.String, Str: answer}
				replaced = true
			}
		}
		if !replaced {
			obj.Obj = append(obj.Obj, jsonx.Member{Key: key, Value: &jsonx.Value{Kind: jsonx.String, Str: answer}})
		}
	}
	b, _ := obj.MarshalJSON()
	return b
}

// textAnswers is text as the free-text answer to every question (never
// parsed as JSON, unlike an app answer).
func textAnswers(text string, qs []events.Question) json.RawMessage {
	obj := &jsonx.Value{Kind: jsonx.Object, Obj: []jsonx.Member{}}
	seen := map[string]bool{}
	for _, q := range qs {
		key := q.Question
		if key == "" {
			key = q.Header
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		obj.Obj = append(obj.Obj, jsonx.Member{Key: key, Value: &jsonx.Value{Kind: jsonx.String, Str: text}})
	}
	b, _ := obj.MarshalJSON()
	return b
}

func (s *Session) finishQuestionLocked(pq *pendingQuestion, answer string, reply bool) {
	s.finishQuestionWithLocked(pq, answersFor(answer, pq.questions), reply)
}

func (s *Session) finishQuestionWithLocked(pq *pendingQuestion, answers json.RawMessage, reply bool) {
	if pq.timer != nil {
		pq.timer.Stop()
	}
	s.emit(events.QuestionAnswer{Type: "question_answer", Answers: answers})
	if !reply {
		return
	}
	pq.proc.Respond(pq.reqID, map[string]any{
		"behavior":     "allow",
		"updatedInput": map[string]json.RawMessage{"questions": pq.rawQuestions, "answers": answers},
		"toolUseID":    pq.toolUseID,
	})
}

// RespondQuestion answers the oldest pending question (dropped if none).
func (s *Session) RespondQuestion(answer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if answer == "" {
		answer = "skip"
	}
	if len(s.questions) == 0 {
		s.log.Info("question answer with nothing pending, dropped")
		return
	}
	pq := s.questions[0]
	s.questions = s.questions[1:]
	s.finishQuestionLocked(pq, answer, true)
	s.talkUpdateLocked()
}

// cancelRequestLocked: the CLI withdrew a can_use_tool request.
func (s *Session) cancelRequestLocked(reqID string) {
	for _, pp := range append([]*pendingPerm(nil), s.perms...) {
		if pp.reqID == reqID {
			s.removePermLocked(pp)
			s.finishPermLocked(pp, "deny", false)
		}
	}
	for _, pq := range append([]*pendingQuestion(nil), s.questions...) {
		if pq.reqID == reqID {
			s.removeQuestionLocked(pq)
			s.finishQuestionLocked(pq, "skip", false)
		}
	}
}

// resolveAllPendingLocked answers every pending prompt with its default.
func (s *Session) resolveAllPendingLocked(reply bool) {
	perms, qs := s.perms, s.questions
	s.perms, s.questions = nil, nil
	for _, pp := range perms {
		s.finishPermLocked(pp, "deny", reply)
	}
	for _, pq := range qs {
		s.finishQuestionLocked(pq, "skip", reply)
	}
}

// keepPendingForSubagentsLocked runs at turn end. The main agent can't end
// a turn with its own request open, so whatever is still pending is a
// subagent's: it stays open (its timeout keeps running) and no longer
// counts as blocking the next turn.
func (s *Session) keepPendingForSubagentsLocked() {
	if len(s.perms)+len(s.questions) == 0 {
		return
	}
	for _, pp := range s.perms {
		pp.blocksTurn = false
	}
	for _, pq := range s.questions {
		pq.blocksTurn = false
	}
	s.log.Info("requests stay open after the turn (subagents)", "permissions", len(s.perms), "questions", len(s.questions))
}

// dropPendingLocked forgets open requests without answering them, for a
// process that is being closed (its requests end with it).
func (s *Session) dropPendingLocked(why string) {
	if n := len(s.perms) + len(s.questions); n > 0 {
		s.log.Info("dropping open requests", "count", n, "why", why)
	}
	for _, pp := range s.perms {
		if pp.timer != nil {
			pp.timer.Stop()
		}
	}
	for _, pq := range s.questions {
		if pq.timer != nil {
			pq.timer.Stop()
		}
	}
	s.perms, s.questions = nil, nil
}

// Interrupt stops the running turn. Between turns it does nothing: the CLI's
// interrupt would stop running background agents (with
// perTaskStopAffordance it spares them, but there is no turn to stop).
func (s *Session) Interrupt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interruptLocked()
}

func (s *Session) interruptLocked() {
	p := s.proc
	if !s.busy {
		s.log.Info("interrupt while idle ignored", "backgroundTasks", len(s.bgTasks), "agents", s.mapper.RunningAgents())
		s.resolveAllPendingLocked(p != nil) // a subagent's open permission/question: deny
		return
	}
	s.interrupted = true
	s.talkStopTimerLocked()
	var wait func(time.Duration) (json.RawMessage, error)
	if p != nil && !s.launching {
		wait = p.StartRequest("interrupt", nil)
	}
	s.resolveAllPendingLocked(p != nil)
	if wait != nil {
		go func() {
			defer guard.Recover(s.log, "interrupt")
			if _, err := wait(10 * time.Second); err != nil {
				s.log.Warn("interrupt failed", "err", err)
			}
		}()
	}
}

// close stops the process (server shutdown).
func (s *Session) close() {
	s.mu.Lock()
	p := s.proc
	s.gen++
	s.proc = nil
	s.clearIdleTimerLocked()
	s.talkStopTimerLocked()
	s.dropPendingLocked("shutdown")
	s.mu.Unlock()
	if p != nil {
		p.Close()
	}
}

// decodeLenient unmarshals a CLI line. A field of an unexpected type (the
// CLI's format drifts between versions) leaves that field empty instead of
// losing the whole line (logged); only malformed JSON is an error.
func decodeLenient(log *slog.Logger, line []byte, v any) error {
	err := json.Unmarshal(line, v)
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		log.Warn("cli line field of unexpected type ignored", "field", te.Field, "err", err)
		return nil
	}
	return err
}

// errorTexts reads a result's errors: strings, or objects with a message.
func errorTexts(raw json.RawMessage) []string {
	var items []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		var str string
		if json.Unmarshal(it, &str) == nil {
			out = append(out, str)
			continue
		}
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(it, &obj) == nil && obj.Message != "" {
			out = append(out, obj.Message)
			continue
		}
		out = append(out, string(it))
	}
	return out
}

func (s *Session) health() SessionHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := SessionHealth{ID: s.id, State: s.stateLocked(), Cwd: s.cwd, Launching: s.launching, Queued: len(s.queue),
		Permissions: len(s.perms), Questions: len(s.questions), BackgroundTasks: len(s.bgTasks), Agents: s.mapper.RunningAgents()}
	if s.proc != nil {
		h.Pid = s.proc.Pid()
	}
	return h
}

// redundantNotice reports a Claude Code notification that only says a
// permission prompt or question is waiting: the glasses already show that
// prompt itself ("Claude needs your permission to use AskUserQuestion").
func redundantNotice(kind, message string) bool {
	return kind == "permission_prompt" || strings.HasPrefix(message, "Claude needs your permission")
}
