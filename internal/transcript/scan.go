// Package transcript reads Claude Code session transcripts
// (~/.claude/projects/*/<uuid>.jsonl) cheaply: head/tail reads only, cached
// by (size, mtime). Semantics follow the Agent SDK's listSessions /
// getSessionMessages (docs/even-protocol.md §3.3, §3.4).
package transcript

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// HeadTailBytes is the SDK's read window (64 KiB) for list metadata.
const HeadTailBytes = 64 * 1024

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsSessionID reports whether s looks like a session UUID.
func IsSessionID(s string) bool { return uuidRE.MatchString(s) }

// ClaudeDir returns $CLAUDE_CONFIG_DIR or ~/.claude (%USERPROFILE%\.claude on Windows).
func ClaudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude"
	}
	return filepath.Join(home, ".claude")
}

// readHeadTail returns the first and last n bytes of f (tail == head when
// the file fits in n bytes).
func readHeadTail(path string, n int64) (head, tail []byte, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, 0, err
	}
	size = st.Size()
	hn := min(size, n)
	head = make([]byte, hn)
	if _, err := io.ReadFull(f, head); err != nil {
		return nil, nil, 0, err
	}
	if size <= n {
		return head, head, size, nil
	}
	tail = make([]byte, n)
	if _, err := f.ReadAt(tail, size-n); err != nil && err != io.EOF {
		return nil, nil, 0, err
	}
	return head, tail, size, nil
}

// readTail returns the last n bytes of path.
func readTail(path string, n int64) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()
	ln := min(size, n)
	buf := make([]byte, ln)
	if _, err := f.ReadAt(buf, size-ln); err != nil && err != io.EOF {
		return nil, 0, err
	}
	return buf, size, nil
}

// unescapeJS decodes a raw JSON string body when it contains escapes.
func unescapeJS(raw []byte) string {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw)
	}
	var s string
	q := make([]byte, 0, len(raw)+2)
	q = append(q, '"')
	q = append(q, raw...)
	q = append(q, '"')
	if err := json.Unmarshal(q, &s); err != nil {
		return string(raw)
	}
	return s
}

// scanStringAt reads a JSON string body starting at i (just after the
// opening quote); returns the body end index or -1.
func scanStringAt(text []byte, i int) int {
	for c := i; c < len(text); c++ {
		switch text[c] {
		case '\\':
			c++
		case '"':
			return c
		}
	}
	return -1
}

// lastStringField is the SDK's Qt(): the value of the LAST `"key":"…"` (or
// `"key": "…"`) occurrence anywhere in text. ok=false when absent.
func lastStringField(text []byte, key string) (string, bool) {
	pats := [][]byte{[]byte(`"` + key + `":"`), []byte(`"` + key + `": "`)}
	best := -1
	var val string
	for _, p := range pats {
		from := 0
		for {
			a := bytes.Index(text[from:], p)
			if a < 0 {
				break
			}
			a += from
			start := a + len(p)
			end := scanStringAt(text, start)
			if end < 0 {
				break
			}
			if a > best {
				best = a
				val = unescapeJS(text[start:end])
			}
			from = end + 1
		}
	}
	return val, best >= 0
}

// firstStringField is the SDK's nl(): the FIRST occurrence.
func firstStringField(text []byte, key string) (string, bool) {
	for _, p := range [][]byte{[]byte(`"` + key + `":"`), []byte(`"` + key + `": "`)} {
		a := bytes.Index(text, p)
		if a < 0 {
			continue
		}
		start := a + len(p)
		end := scanStringAt(text, start)
		if end < 0 {
			continue
		}
		return unescapeJS(text[start:end]), true
	}
	return "", false
}

// lines splits text on '\n'.
func lines(text []byte) [][]byte { return bytes.Split(text, []byte("\n")) }

func containsAny(line []byte, subs ...string) bool {
	for _, s := range subs {
		if bytes.Contains(line, []byte(s)) {
			return true
		}
	}
	return false
}

// lastTypedField is the SDK's yCe(): scanning lines from the end, the first
// parseable object with type==typ (if set) whose key is a string.
func lastTypedField(text []byte, key, typ string) string {
	ls := lines(text)
	for i := len(ls) - 1; i >= 0; i-- {
		l := ls[i]
		if !bytes.Contains(l, []byte(`"`+key+`":`)) {
			continue
		}
		if typ != "" && !bytes.Contains(l, []byte(`"type":"`+typ+`"`)) {
			continue
		}
		var m map[string]any
		if json.Unmarshal(l, &m) != nil {
			continue
		}
		if typ != "" && m["type"] != typ {
			continue
		}
		if s, ok := m[key].(string); ok {
			return s
		}
	}
	return ""
}

var (
	commandNameRE = regexp.MustCompile(`<command-name>(.*?)</command-name>`)
	bashInputRE   = regexp.MustCompile(`<bash-input>([\s\S]*?)</bash-input>`)
	skipPromptRE  = regexp.MustCompile(`^(?:\s*<[a-z][\w-]*[\s>]|\[Request interrupted by user[^\]]*\])`)
)

type promptState struct{ commandFallback string }

// promptFromEntry is the SDK's Bi(): display text of a user entry or "" with
// ok=false when the entry yields nothing.
func promptFromEntry(e map[string]any, st *promptState) (string, bool) {
	if e["type"] != "user" {
		return "", false
	}
	if e["isMeta"] == true || e["isCompactSummary"] == true {
		return "", false
	}
	msg, _ := e["message"].(map[string]any)
	if msg == nil {
		return "", false
	}
	var texts []string
	switch c := msg["content"].(type) {
	case string:
		texts = append(texts, c)
	case []any:
		for _, b := range c {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if bm["type"] == "tool_result" {
				return "", false
			}
			if bm["type"] == "text" {
				if s, ok := bm["text"].(string); ok {
					texts = append(texts, s)
				}
			}
		}
	}
	for _, s := range texts {
		t := strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
		if t == "" {
			continue
		}
		if m := commandNameRE.FindStringSubmatch(t); m != nil {
			if st.commandFallback == "" {
				st.commandFallback = m[1]
			}
			continue
		}
		if m := bashInputRE.FindStringSubmatch(t); m != nil {
			return "! " + strings.TrimSpace(m[1]), true
		}
		if skipPromptRE.MatchString(t) {
			continue
		}
		if utf8.RuneCountInString(t) > 200 {
			r := []rune(t)
			t = strings.TrimSpace(string(r[:200])) + "…"
		}
		return t, true
	}
	return "", false
}

// firstPrompt is the SDK's fm() over the head bytes.
func firstPrompt(head []byte) string {
	st := &promptState{}
	for _, l := range lines(head) {
		if !containsAny(l, `"type":"user"`, `"type": "user"`) {
			continue
		}
		if bytes.Contains(l, []byte(`"tool_result"`)) {
			continue
		}
		if containsAny(l, `"isMeta":true`, `"isMeta": true`, `"isCompactSummary":true`, `"isCompactSummary": true`) {
			continue
		}
		var e map[string]any
		if json.Unmarshal(l, &e) != nil {
			continue
		}
		if p, ok := promptFromEntry(e, st); ok {
			return p
		}
	}
	return st.commandFallback
}

// mediaFallback is the SDK's tL(): "Image"/"Document" for media-only prompts.
func mediaFallback(head []byte) string {
	for _, l := range lines(head) {
		if !containsAny(l, `"type":"user"`, `"type": "user"`) {
			continue
		}
		if bytes.Contains(l, []byte(`"tool_result"`)) || containsAny(l, `"isMeta":true`, `"isMeta": true`) {
			continue
		}
		if containsAny(l, `"type":"image"`, `"type": "image"`) {
			return "Image"
		}
		if containsAny(l, `"type":"document"`, `"type": "document"`) {
			return "Document"
		}
	}
	return ""
}
