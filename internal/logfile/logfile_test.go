package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "even-cc.log")
	l, err := Open(p, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 39) + "\n" // 40 bytes
	for i := 0; i < 10; i++ {
		if _, err := l.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	size := func(name string) int64 {
		st, err := os.Stat(name)
		if err != nil {
			return -1
		}
		return st.Size()
	}
	// 10 lines, 2 per file: current has 2, .1 and .2 have 2 each, .3 never exists
	if size(p) != 80 || size(p+".1") != 80 || size(p+".2") != 80 || size(p+".3") != -1 {
		t.Errorf("sizes %d %d %d %d", size(p), size(p+".1"), size(p+".2"), size(p+".3"))
	}
	// reopening appends and counts the existing size
	l, _ = Open(p, 100, 2)
	l.Write([]byte(line))
	l.Close()
	if size(p) != 40 || size(p+".1") != 80 {
		t.Errorf("after reopen %d %d", size(p), size(p+".1"))
	}
	// a single write bigger than the cap still lands
	l, _ = Open(p, 10, 1)
	if n, err := l.Write([]byte(line)); n != 40 || err != nil {
		t.Errorf("big write %d %v", n, err)
	}
	l.Close()
}
