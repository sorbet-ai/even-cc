package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestIDFloorFitsInt32(t *testing.T) {
	if f := idFloor(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)); f < 25_000_000 || f > 27_000_000 {
		t.Errorf("floor %d", f)
	}
	if f := idFloor(time.Date(2090, 1, 1, 0, 0, 0, 0, time.UTC)); f >= 1<<31-1 {
		t.Errorf("floor overflows int32 by 2090: %d", f)
	}
	if idFloor(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) != 1 {
		t.Error("floor before the epoch")
	}
}

func firstFrameID(t *testing.T, c *Client) int64 {
	t.Helper()
	select {
	case f := <-c.C:
		line := strings.SplitN(string(f), "\n", 2)[0]
		n, err := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		if err != nil {
			t.Fatalf("frame %q", f)
		}
		return n
	case <-time.After(time.Second):
		t.Fatal("no frame")
	}
	return 0
}

type ev struct {
	Type string `json:"type"`
}

func TestIDsMonotonicAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 10, 30, 11, 47, 0, 0, time.UTC)
	start := func() *Hub {
		h := New(nil)
		h.PersistIDs(path)
		h.ids.now = func() time.Time { return now }
		return h
	}

	h1 := start()
	floor := idFloor(now)
	if id := h1.Emit("s1", ev{"status"}); id != floor {
		t.Fatalf("first id %d, want the floor %d", id, floor)
	}
	var last int64
	for i := 0; i < 2500; i++ { // crosses two reservation blocks
		last = h1.Emit("s1", ev{"text_delta"})
	}
	if last != floor+2500 {
		t.Fatalf("last %d", last)
	}

	// "restart" a second later without any shutdown (as after a kill)
	now = now.Add(time.Second)
	h2 := start()
	c := h2.Subscribe("s1", false)
	id := h2.Emit("s1", ev{"status"})
	if id <= last {
		t.Fatalf("id went back after restart: %d <= %d", id, last)
	}
	if got := firstFrameID(t, c); got != id {
		t.Errorf("frame id %d, want %d", got, id)
	}
	// /api/messages?after= still works with big ids
	if m := string(h2.Messages("s1", id-1)); !strings.HasPrefix(m, `[{"id":`+strconv.FormatInt(id, 10)) {
		t.Errorf("messages %s", m)
	}
	// a session unknown to the file starts at the floor
	if id := h2.Emit("s2", ev{"status"}); id != idFloor(now) {
		t.Errorf("new session id %d", id)
	}

	// the file holds reservations, no event data
	b, _ := os.ReadFile(path)
	var f idFile
	if err := json.Unmarshal(b, &f); err != nil || f.Sessions["s1"].Reserved < id || len(f.Sessions) != 2 {
		t.Errorf("state %s %v", b, err)
	}

	// stale entries are pruned, unreadable files tolerated
	now = now.Add(idStateTTL + time.Hour)
	h3 := start()
	h3.Emit("s3", ev{"status"})
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), `"s1"`) || !strings.Contains(string(b), `"s3"`) {
		t.Errorf("not pruned: %s", b)
	}
	_ = os.WriteFile(path, []byte("{garbage"), 0o600)
	h4 := start()
	if id := h4.Emit("s1", ev{"status"}); id != idFloor(now) {
		t.Errorf("after a corrupt file: %d", id)
	}
}

func TestIDsWithoutStateStartAtOne(t *testing.T) {
	h := New(nil)
	if id := h.Emit("s", ev{"status"}); id != 1 {
		t.Errorf("id %d", id)
	}
}
