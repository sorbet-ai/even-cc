package guard

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestRecoverAndGo(t *testing.T) {
	var buf syncBuf
	log := slog.New(slog.NewTextHandler(&buf, nil))
	before := Panics.Load()
	func() {
		defer Recover(log, "sync call")
		var m map[string]int
		//lint:ignore SA5000 the panic is the point
		m["x"] = 1 // nil map write
	}()
	out := buf.String()
	if Panics.Load() != before+1 || !strings.Contains(out, "sync call") || !strings.Contains(out, "assignment to entry in nil map") || !strings.Contains(out, "guard_test.go") {
		t.Fatalf("log %s", out)
	}
	Go(log, "async goroutine", func() { panic("boom") })
	deadline := time.Now().Add(2 * time.Second)
	for Panics.Load() != before+2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(buf.String(), "async goroutine") {
		t.Errorf("log %s", buf.String())
	}
}
