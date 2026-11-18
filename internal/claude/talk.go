package claude

import (
	"strings"
	"time"
	"unicode"

	"even-cc/internal/events"
	"even-cc/internal/guard"
)

// Talk while busy.
//
// The Even app locks voice input while a session is busy (hold-to-speak
// becomes Cancel), but the CLI takes user messages mid-turn and folds them in
// at the next tool boundary. So while a main-agent tool has been running for
// Display.TalkWhileBusyAfter with nothing else going on, the app is told
// "idle"; busy is re-sent before anything else reaches it (tool result, new
// text or thinking, permission prompt, result). At most one idle/busy pair per
// tool. All methods need s.mu.

// talkEligibleLocked: a main-agent tool that hasn't had its pair yet is
// executing, and nothing would contradict an idle.
func (s *Session) talkEligibleLocked() bool {
	if !s.m.Display().TalkWhileBusy {
		return false
	}
	if !s.busy || !s.busyEmitted || s.resultSent || s.interrupted || s.launching || s.proc == nil {
		return false
	}
	if len(s.perms) > 0 || len(s.questions) > 0 || s.mapper.Streaming() {
		return false
	}
	for id := range s.talkTools {
		if !s.talkUsed[id] {
			return true
		}
	}
	return false
}

// talkUpdateLocked arms the timer when the conditions start to hold and
// stops it when they stop holding. Frames that change nothing leave a running
// timer alone (subagent traffic must not postpone the idle).
func (s *Session) talkUpdateLocked() {
	if s.talkIdle {
		return
	}
	if !s.talkEligibleLocked() {
		s.talkStopTimerLocked()
		return
	}
	if s.talkTimer != nil {
		return
	}
	seq, gen := s.turnSeq, s.gen
	var t *time.Timer
	t = time.AfterFunc(s.m.Display().TalkWhileBusyAfter, func() {
		defer guard.Recover(s.log, "talk while busy timer")
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.talkTimer != t {
			return
		}
		s.talkTimer = nil
		if s.turnSeq != seq || s.gen != gen || s.talkIdle || !s.talkEligibleLocked() {
			return
		}
		s.talkSleepLocked()
	})
	s.talkTimer = t
}

func (s *Session) talkStopTimerLocked() {
	if s.talkTimer != nil {
		s.talkTimer.Stop()
		s.talkTimer = nil
	}
}

// talkSleepLocked tells the app idle while the turn goes on.
func (s *Session) talkSleepLocked() {
	n := 0
	for id := range s.talkTools {
		if s.talkUsed == nil {
			s.talkUsed = map[string]bool{}
		}
		s.talkUsed[id] = true
		n++
	}
	s.talkIdle = true
	s.mapper.SetQuiet(true)
	s.log.Info("talk while busy: idle sent while a tool runs", "tools", n)
	s.m.hub.Emit(s.id, events.NewStatus("idle", s.id))
}

// talkWakeLocked re-sends busy if the app was told idle mid-turn, then what
// was held meanwhile.
func (s *Session) talkWakeLocked(why string) {
	if !s.talkIdle {
		return
	}
	s.talkIdle = false
	s.mapper.SetQuiet(false)
	s.log.Info("talk while busy: busy again", "on", why)
	s.m.hub.Emit(s.id, events.NewStatus("busy", s.id))
	s.mapper.FlushHeld()
}

// talkGateLocked runs before every session event: content while the app was
// told idle gets busy first. Notifications pass (they don't make the app
// busy); a status busy/idle ends the talk idle itself.
func (s *Session) talkGateLocked(ev any) {
	if !s.talkIdle {
		return
	}
	switch e := ev.(type) {
	case events.Notification:
		return
	case events.Status:
		if e.State == "busy" || e.State == "idle" {
			s.talkIdle = false
			s.mapper.SetQuiet(false)
			return
		}
	}
	s.talkWakeLocked("event")
}

func (s *Session) talkResetLocked() {
	s.talkStopTimerLocked()
	s.talkIdle = false
	s.talkTools = nil
	s.talkUsed = nil
	s.mapper.SetQuiet(false)
}

// talkToolsStartedLocked records main-agent tool_use blocks (executing until
// their tool_result).
func (s *Session) talkToolsStartedLocked(blocks []contentBlock) {
	for _, b := range blocks {
		if b.Type == "tool_use" && b.ID != "" {
			if s.talkTools == nil {
				s.talkTools = map[string]bool{}
			}
			s.talkTools[b.ID] = true
		}
	}
}

func (s *Session) talkToolsEndedLocked(blocks []contentBlock) {
	for _, b := range blocks {
		if b.Type == "tool_result" {
			delete(s.talkTools, b.ToolUseID)
		}
	}
}

// isStopWord: the whole prompt is "stop", "cancel", "stop it" or "halt"
// (any case, surrounding spaces and trailing punctuation ignored).
func isStopWord(text string) bool {
	t := strings.TrimRightFunc(strings.ToLower(text), func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) })
	switch strings.Join(strings.Fields(t), " ") {
	case "stop", "cancel", "stop it", "halt":
		return true
	}
	return false
}
