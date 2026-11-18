package transcript

import (
	"bytes"
	"encoding/json"
	"os"
)

// HistoryItem is one /api/sessions/:id/history row.
type HistoryItem struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type chainEntry struct {
	Type        string          `json:"type"`
	UUID        string          `json:"uuid"`
	ParentUUID  *string         `json:"parentUuid"`
	IsSidechain bool            `json:"isSidechain"`
	IsMeta      bool            `json:"isMeta"`
	TeamName    string          `json:"teamName"`
	Message     json.RawMessage `json:"message"`
	pos         int
}

// History returns the last `limit` text items of the session's main chain
// (latest leaf back via parentUuid), oldest first. Reads the transcript tail,
// growing the window only while the chain leaves it.
func (s *Store) History(id string, limit int) ([]HistoryItem, error) {
	items, err := s.history(id, limit)
	if items == nil {
		items = []HistoryItem{}
	}
	return items, err
}

func (s *Store) history(id string, limit int) ([]HistoryItem, error) {
	if limit <= 0 {
		return nil, nil
	}
	p := s.FindFile(id)
	if p == "" {
		return nil, nil
	}
	fi, err := os.Stat(p)
	if err != nil {
		return nil, nil
	}
	key := p + "\x00" + string(rune(limit))
	s.mu.Lock()
	h, ok := s.hist[key]
	s.mu.Unlock()
	if ok && h.size == fi.Size() && h.mtime == fi.ModTime().UnixNano() {
		return h.items, nil
	}

	window := int64(1 << 20)
	var items []HistoryItem
	for {
		tail, size, err := readTail(p, window)
		if err != nil {
			return nil, err
		}
		whole := int64(len(tail)) >= size
		var complete bool
		items, complete = chainItems(tail, !whole, limit)
		if complete || whole {
			break
		}
		window *= 4
	}
	s.mu.Lock()
	s.hist[key] = histEntry{size: fi.Size(), mtime: fi.ModTime().UnixNano(), items: items}
	s.mu.Unlock()
	return items, nil
}

// chainItems walks the main chain inside text. complete=false means the
// chain left the window before enough items were found.
func chainItems(text []byte, partial bool, limit int) ([]HistoryItem, bool) {
	ls := lines(text)
	if partial && len(ls) > 0 {
		ls = ls[1:] // first line is cut
	}
	byID := make(map[string]*chainEntry, len(ls))
	var leaf *chainEntry
	for i, l := range ls {
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		if !containsAny(l, `"uuid"`) {
			continue
		}
		var e chainEntry
		if json.Unmarshal(l, &e) != nil || e.UUID == "" {
			continue
		}
		switch e.Type {
		case "user", "assistant", "progress", "system", "attachment":
		default:
			continue
		}
		e.pos = i
		ce := e
		byID[e.UUID] = &ce
		if (e.Type == "user" || e.Type == "assistant") && !e.IsSidechain && e.TeamName == "" {
			leaf = &ce
		}
	}
	if leaf == nil {
		return nil, !partial
	}
	var rev [][]HistoryItem // per entry, newest entry first
	count := 0
	seen := map[string]bool{}
	cur := leaf
	for cur != nil {
		if seen[cur.UUID] {
			break
		}
		seen[cur.UUID] = true
		if (cur.Type == "user" || cur.Type == "assistant") && !cur.IsMeta && !cur.IsSidechain && cur.TeamName == "" {
			if its := entryItems(cur); len(its) > 0 {
				rev = append(rev, its)
				count += len(its)
				if count >= limit {
					break
				}
			}
		}
		if cur.ParentUUID == nil || *cur.ParentUUID == "" {
			break
		}
		next := byID[*cur.ParentUUID]
		if next == nil {
			if partial {
				return nil, false
			}
			break
		}
		cur = next
	}
	var out []HistoryItem
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i]...)
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, true
}

func entryItems(e *chainEntry) []HistoryItem {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(e.Message, &msg) != nil || len(msg.Content) == 0 {
		return nil
	}
	var str string
	if json.Unmarshal(msg.Content, &str) == nil {
		if str == "" {
			return nil
		}
		return []HistoryItem{{Role: e.Type, Text: str}}
	}
	var blocks []struct {
		Type string  `json:"type"`
		Text *string `json:"text"`
	}
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return nil
	}
	var out []HistoryItem
	for _, b := range blocks {
		if b.Type == "text" {
			t := ""
			if b.Text != nil {
				t = *b.Text
			}
			out = append(out, HistoryItem{Role: e.Type, Text: t})
		}
	}
	return out
}
