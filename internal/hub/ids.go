package hub

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Event ids across restarts.
//
// even-terminal numbers each session's events from 1 and forgets them on
// restart. The Even app keeps its SSE connection's state across a server
// restart (it reconnects within a second, without being relaunched), and
// after an even-cc restart it showed nothing at all for a session whose
// previous process had sent hundreds of events: consistent with the app
// ignoring event ids it has already seen. So with an id state file, ids of a
// session never go back:
//
//   - A session's first id in this process is max(last reserved id + 1,
//     idFloor()). idFloor is the seconds since 2026-01-01 UTC (about 26 M in
//     late 2026), above anything a process that started at 1 has sent, and
//     it stays below 2^31 until 2094, so an app parsing ids as int32 is fine.
//   - Ids are reserved in blocks of idBlock: the state file is written when a
//     session first emits and then once every idBlock events, before an id
//     past the reservation is used, so even a killed process never reuses an
//     id. A restart skips the rest of the block.
//   - Entries untouched for idStateTTL are pruned; the floor alone keeps such
//     a session monotonic (it would need more than one event per second of
//     downtime to collide).

const (
	idBlock    = 1000
	idStateTTL = 30 * 24 * time.Hour
)

var idEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// idFloor is the lowest first id for a session in a process started now.
func idFloor(now time.Time) int64 {
	f := int64(now.Sub(idEpoch) / time.Second)
	if f < 1 {
		return 1
	}
	return f
}

type idRecord struct {
	Reserved int64 `json:"reserved"` // highest id that may have been used
	Updated  int64 `json:"updated"`  // unix seconds
}

type idState struct {
	path     string
	sessions map[string]idRecord
	now      func() time.Time
	warned   bool
}

type idFile struct {
	Version  int                 `json:"version"`
	Sessions map[string]idRecord `json:"sessions"`
}

// PersistIDs makes event ids monotonic across restarts, keeping the
// reservations in path (e.g. ~/.even-cc/state.json). Call before any Emit.
// A missing or unreadable file only loses the reservations (logged); the
// time floor still applies.
func (h *Hub) PersistIDs(path string) {
	st := &idState{path: path, sessions: map[string]idRecord{}, now: time.Now}
	if b, err := os.ReadFile(path); err == nil {
		var f idFile
		if err := json.Unmarshal(b, &f); err != nil {
			if h.log != nil {
				h.log.Warn("event id state unreadable, starting from the time floor", "path", path, "err", err)
			}
		} else if f.Sessions != nil {
			st.sessions = f.Sessions
		}
	} else if !os.IsNotExist(err) && h.log != nil {
		h.log.Warn("event id state unreadable, starting from the time floor", "path", path, "err", err)
	}
	h.mu.Lock()
	h.ids = st
	h.mu.Unlock()
}

// first is the first id for a session new in this process.
func (st *idState) first(sessionID string) int64 {
	n := idFloor(st.now())
	if r, ok := st.sessions[sessionID]; ok && r.Reserved+1 > n {
		n = r.Reserved + 1
	}
	return n
}

// reserve records that ids up to reserved may be used, and saves the file
// (h.mu held; this happens once per session start and per idBlock events).
func (st *idState) reserve(sessionID string, reserved int64, log *slog.Logger) {
	now := st.now()
	st.sessions[sessionID] = idRecord{Reserved: reserved, Updated: now.Unix()}
	cutoff := now.Add(-idStateTTL).Unix()
	for k, r := range st.sessions {
		if r.Updated < cutoff {
			delete(st.sessions, k)
		}
	}
	if err := st.save(); err != nil {
		if !st.warned && log != nil {
			log.Warn("could not save event id state; ids still start from the time floor after a restart", "path", st.path, "err", err)
		}
		st.warned = true
		return
	}
	st.warned = false
}

func (st *idState) save() error {
	b, err := json.Marshal(idFile{Version: 1, Sessions: st.sessions})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o700); err != nil {
		return err
	}
	// Synced before the rename: after a crash the file holds either the old
	// or the new reservations, never an empty or torn one.
	tmp := st.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}
