package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Claude Code sessions take messages from other local sessions on an inbox
// (a Unix socket, or a named pipe on Windows) named in their registry entry
// (messagingSocketPath). The wire is one JSON object per line: an auth line
// with the session's peer token (from <claudeDir>/sessions/<pid>.<hash>.key),
// then {"type":"user","message":{"content":...}}. The receiving Claude sees it
// as a message from another session, not as the user's own turn, so it reads
// it as information or a request, and may still check with the person there.

// messageFrom is the sender name the receiving session shows.
const messageFrom = "even-cc (the user, on their glasses)"

// MessageElsewhere sends text into the session another process holds, as a
// cross-session message. The holder's transcript shows it once it is read.
func (m *Manager) MessageElsewhere(id, text string) error {
	e, held := m.HeldElsewhere(id)
	if !held {
		return &HTTPError{Code: 409, Msg: "That session isn't open anywhere else: talk to it directly."}
	}
	if err := m.SendToProcess(e.Pid, id, text); err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			he.Msg = "Can't message it in " + e.Where + ": " + he.Msg
		}
		return err
	}
	m.log.Info("messaged a session held elsewhere", "session", id, "pid", e.Pid, "where", e.Where, "chars", len(text))
	return nil
}

// SendToProcess writes text to the inbox of the Claude Code process pid, for
// its session sessionID.
func (m *Manager) SendToProcess(pid int, sessionID, text string) error {
	sock, err := m.inboxPath(pid)
	if err != nil {
		return &HTTPError{Code: 409, Msg: err.Error()}
	}
	token, err := m.peerToken(pid)
	if err != nil {
		return &HTTPError{Code: 409, Msg: err.Error()}
	}
	lines, err := messageLines(token, sessionID, text)
	if err != nil {
		return err
	}
	conn, err := dialInbox(sock, 3*time.Second)
	if err != nil {
		return &HTTPError{Code: 502, Msg: err.Error()}
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(lines); err != nil {
		return &HTTPError{Code: 502, Msg: err.Error()}
	}
	return nil
}

// messageLines is the auth line and the message line.
func messageLines(token, sessionID, text string) ([]byte, error) {
	if text == "" {
		return nil, &HTTPError{Code: 400, Msg: "Missing 'text' field"}
	}
	auth, _ := json.Marshal(map[string]string{"type": "auth", "token": token})
	msg, err := json.Marshal(map[string]any{
		"type":       "user",
		"session_id": sessionID, // the inbox drops it if the process moved on to another session
		"from":       messageFrom,
		"priority":   "next",
		"message":    map[string]string{"content": text},
	})
	if err != nil {
		return nil, err
	}
	return append(append(append(auth, '\n'), msg...), '\n'), nil
}

// inboxPath is the messaging socket the process registered.
func (m *Manager) inboxPath(pid int) (string, error) {
	b, err := os.ReadFile(filepath.Join(m.opts.ClaudeDir, "sessions", strconv.Itoa(pid)+".json"))
	if err != nil {
		return "", errors.New("its session registry entry is gone")
	}
	var e struct {
		Sock string `json:"messagingSocketPath"`
	}
	if json.Unmarshal(b, &e) != nil || e.Sock == "" {
		return "", errors.New("that Claude Code version has no message inbox")
	}
	return e.Sock, nil
}

// peerToken reads the inbox's auth token from <pid>.<hash>.key.
func (m *Manager) peerToken(pid int) (string, error) {
	keys, _ := filepath.Glob(filepath.Join(m.opts.ClaudeDir, "sessions", strconv.Itoa(pid)+".*.key"))
	for _, k := range keys {
		b, err := os.ReadFile(k)
		if err != nil {
			continue
		}
		var v struct {
			PeerToken string `json:"peerToken"`
		}
		if json.Unmarshal(b, &v) == nil && len(v.PeerToken) == 32 {
			return v.PeerToken, nil
		}
	}
	return "", fmt.Errorf("no inbox key for pid %d", pid)
}
