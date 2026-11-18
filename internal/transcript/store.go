package transcript

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"even-cc/internal/jsonx"
)

// SessionInfo is one row of the session list.
type SessionInfo struct {
	ID    string
	Title string // already cut to 64 UTF-16 units
	Cwd   string
	Mtime int64 // ms since epoch
	Path  string
}

type metaEntry struct {
	size, mtime int64
	info        *SessionInfo // nil: not listable
}

type histEntry struct {
	size, mtime int64
	items       []HistoryItem
}

// Store caches transcript metadata. Safe for concurrent use.
type Store struct {
	projects string

	mu       sync.Mutex
	meta     map[string]metaEntry
	pathByID map[string]string
	hist     map[string]histEntry
}

// NewStore reads transcripts under <claudeDir>/projects.
func NewStore(claudeDir string) *Store {
	return &Store{
		projects: filepath.Join(claudeDir, "projects"),
		meta:     map[string]metaEntry{},
		pathByID: map[string]string{},
		hist:     map[string]histEntry{},
	}
}

// ProjectsDir is <claudeDir>/projects.
func (s *Store) ProjectsDir() string { return s.projects }

type candidate struct {
	id    string
	path  string
	mtime int64
	size  int64
}

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// ProjectDirName is Claude Code's directory name for a cwd.
func ProjectDirName(cwd string) string { return nonAlnum.ReplaceAllString(cwd, "-") }

func (s *Store) candidates(cwdFilter string) ([]candidate, error) {
	var dirs []string
	if cwdFilter != "" {
		dirs = []string{filepath.Join(s.projects, ProjectDirName(cwdFilter))}
	} else {
		ents, err := os.ReadDir(s.projects)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		for _, e := range ents {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(s.projects, e.Name()))
			}
		}
	}
	var out []candidate
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			if !IsSessionID(id) {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, candidate{id: id, path: filepath.Join(d, name), mtime: fi.ModTime().UnixMilli(), size: fi.Size()})
		}
	}
	return out, nil
}

// List returns up to limit sessions, newest first (ties: id descending).
func (s *Store) List(limit int, cwdFilter string) ([]SessionInfo, error) {
	cands, err := s.candidates(cwdFilter)
	if err != nil {
		return nil, err
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].mtime != cands[j].mtime {
			return cands[i].mtime > cands[j].mtime
		}
		return cands[i].id > cands[j].id
	})
	seen := map[string]bool{}
	var out []SessionInfo
	for _, c := range cands {
		if limit > 0 && len(out) >= limit {
			break
		}
		info := s.metaFor(c)
		if info == nil || seen[info.ID] {
			continue
		}
		seen[info.ID] = true
		out = append(out, *info)
	}
	return out, nil
}

func (s *Store) metaFor(c candidate) *SessionInfo {
	s.mu.Lock()
	m, ok := s.meta[c.path]
	s.mu.Unlock()
	if ok && m.size == c.size && m.mtime == c.mtime {
		return m.info
	}
	info := computeMeta(c)
	s.mu.Lock()
	s.meta[c.path] = metaEntry{size: c.size, mtime: c.mtime, info: info}
	s.pathByID[c.id] = c.path
	s.mu.Unlock()
	return info
}

func computeMeta(c candidate) *SessionInfo {
	head, tail, _, err := readHeadTail(c.path, HeadTailBytes)
	if err != nil || len(head) == 0 {
		return nil
	}
	firstLine := head
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		firstLine = head[:i]
	}
	if containsAny(firstLine, `"isSidechain":true`, `"isSidechain": true`) {
		return nil
	}
	if continuedElsewhere(c.path, tail) {
		return nil
	}
	title, ok := lastStringField(tail, "customTitle")
	if !ok || title == "" {
		if !ok {
			title = sidecarTitle(c.path, c.id)
		}
		if title == "" {
			title, _ = lastStringField(head, "customTitle")
		}
	}
	if title == "" {
		title, _ = lastStringField(tail, "aiTitle")
	}
	if title == "" {
		title, _ = lastStringField(head, "aiTitle")
	}
	if title == "" {
		title, _ = lastStringField(tail, "lastPrompt")
	}
	if title == "" {
		title, _ = lastStringField(tail, "summary")
	}
	if title == "" {
		title = firstPrompt(head)
	}
	if title == "" {
		title = mediaFallback(head)
	}
	if title == "" {
		return nil
	}
	cwd := lastTypedField(tail, "relocatedCwd", "relocated")
	if cwd == "" {
		cwd, _ = firstStringField(head, "cwd")
	}
	return &SessionInfo{ID: c.id, Title: jsonx.SliceUTF16(title, 64), Cwd: cwd, Mtime: c.mtime, Path: c.path}
}

// sidecarTitle reads <dir>/<id>/custom-title.json.
func sidecarTitle(path, id string) string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(path), id, "custom-title.json"))
	if err != nil {
		return ""
	}
	var v struct {
		CustomTitle string `json:"customTitle"`
	}
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	t := strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, strings.TrimSpace(v.CustomTitle))
	if r := []rune(t); len(r) > 200 {
		t = string(r[:200])
	}
	return strings.TrimSpace(t)
}

// continuedElsewhere: the session was continued in another transcript in the
// same directory (SDK I0/D0), so the SDK hides it.
func continuedElsewhere(path string, tail []byte) bool {
	const marker = `"type":"continued-in"`
	if !bytes.Contains(tail, []byte(marker)) {
		return false
	}
	ls := lines(tail)
	for i := len(ls) - 1; i >= 0; i-- {
		l := ls[i]
		if bytes.Contains(l, []byte(marker)) {
			var v struct {
				ContinuedIn string `json:"continuedInSessionId"`
			}
			if json.Unmarshal(l, &v) != nil || !IsSessionID(v.ContinuedIn) {
				return false
			}
			other := filepath.Join(filepath.Dir(path), v.ContinuedIn+".jsonl")
			h, t, _, err := readHeadTail(other, HeadTailBytes)
			if err != nil {
				return false
			}
			return bytes.Contains(h, []byte(`"parentUuid":`)) || bytes.Contains(t, []byte(`"parentUuid":`))
		}
		if containsAny(l, `"type":"user"`, `"type":"assistant"`) {
			return false
		}
	}
	return false
}

// FindFile returns the transcript path for a session id, or "".
func (s *Store) FindFile(id string) string {
	if !IsSessionID(id) {
		return ""
	}
	s.mu.Lock()
	p := s.pathByID[id]
	s.mu.Unlock()
	if p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	ents, err := os.ReadDir(s.projects)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		cand := filepath.Join(s.projects, e.Name(), id+".jsonl")
		if _, err := os.Stat(cand); err == nil {
			s.mu.Lock()
			s.pathByID[id] = cand
			s.mu.Unlock()
			return cand
		}
	}
	return ""
}

// Info returns list metadata for one session (nil if not found / not listable).
func (s *Store) Info(id string) *SessionInfo {
	p := s.FindFile(id)
	if p == "" {
		return nil
	}
	fi, err := os.Stat(p)
	if err != nil {
		return nil
	}
	return s.metaFor(candidate{id: id, path: p, mtime: fi.ModTime().UnixMilli(), size: fi.Size()})
}

// SessionCwd returns the recorded cwd of a session ("" if unknown).
func (s *Store) SessionCwd(id string) string {
	if info := s.Info(id); info != nil && info.Cwd != "" {
		return info.Cwd
	}
	p := s.FindFile(id)
	if p == "" {
		return ""
	}
	head, _, _, err := readHeadTail(p, HeadTailBytes)
	if err != nil {
		return ""
	}
	cwd, _ := firstStringField(head, "cwd")
	return cwd
}

// LastUUID is the uuid of the last user/assistant entry in the transcript
// (read from a 512 KiB tail), "" if none. err only when the file can't be read.
func (s *Store) LastUUID(id string) (string, error) {
	p := s.FindFile(id)
	if p == "" {
		return "", nil
	}
	tail, _, err := readTail(p, 512*1024)
	if err != nil {
		return "", err
	}
	ls := lines(tail)
	for i := len(ls) - 1; i > 0; i-- {
		var e struct {
			Type string `json:"type"`
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(ls[i], &e) != nil {
			continue
		}
		if (e.Type == "user" || e.Type == "assistant") && e.UUID != "" {
			return e.UUID, nil
		}
	}
	return "", nil
}

// RecentModel returns the model id of the last assistant message among the
// 3 most recently modified sessions ("" if none).
func (s *Store) RecentModel() string {
	list, err := s.List(3, "")
	if err != nil {
		return ""
	}
	for _, info := range list {
		tail, _, err := readTail(info.Path, 512*1024)
		if err != nil {
			continue
		}
		ls := lines(tail)
		for i := len(ls) - 1; i > 0; i-- {
			if !bytes.Contains(ls[i], []byte(`"type":"assistant"`)) {
				continue
			}
			var e struct {
				Type    string `json:"type"`
				Message struct {
					Model string `json:"model"`
				} `json:"message"`
			}
			if json.Unmarshal(ls[i], &e) != nil || e.Type != "assistant" {
				continue
			}
			if m := e.Message.Model; m != "" && !strings.HasPrefix(m, "<") {
				return m
			}
		}
	}
	return ""
}

var dateSuffix = regexp.MustCompile(`-\d{8,}$`)

// FormatModel turns "claude-opus-5-5" into "Opus 5.5" like even-terminal.
func FormatModel(model string) string {
	if model == "" {
		return ""
	}
	m := strings.Replace(model, "claude-", "", 1)
	m = dateSuffix.ReplaceAllString(m, "")
	parts := strings.Split(m, "-")
	name := parts[0]
	if name != "" {
		r := []rune(name)
		r[0] = unicode.ToUpper(r[0])
		name = string(r)
	}
	if len(parts) > 1 {
		return name + " " + strings.Join(parts[1:], ".")
	}
	return name
}

// TurnOpen reports whether the session's last turn looks unfinished: the last
// main-thread entry is a prompt or a tool step, not a finished reply (also
// while a permission prompt waits). Meta entries and slash-command output
// don't count; an interrupted turn is finished. False when unreadable.
func (s *Store) TurnOpen(id string) bool {
	p := s.FindFile(id)
	if p == "" {
		return false
	}
	const n = 512 * 1024
	tail, size, err := readTail(p, n)
	if err != nil {
		return false
	}
	ls := lines(tail)
	first := 1 // a cut tail starts mid-line
	if size <= n {
		first = 0
	}
	for i := len(ls) - 1; i >= first; i-- {
		var e struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			IsMeta      bool   `json:"isMeta"`
			Message     struct {
				StopReason *string         `json:"stop_reason"`
				Content    json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(ls[i], &e) != nil || e.IsSidechain || e.IsMeta {
			continue
		}
		switch e.Type {
		case "assistant":
			return e.Message.StopReason == nil || *e.Message.StopReason == "tool_use"
		case "user":
			var text string
			if json.Unmarshal(e.Message.Content, &text) == nil {
				if strings.HasPrefix(text, "<command-") || strings.HasPrefix(text, "<local-command-") {
					continue
				}
				if strings.HasPrefix(text, "[Request interrupted") {
					return false
				}
			} else if bytes.Contains(e.Message.Content, []byte("[Request interrupted")) {
				return false
			}
			return true
		}
	}
	return false
}
