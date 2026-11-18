package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sorbet-ai/even-cc/internal/config"
)

func TestListenAllPortFallback(t *testing.T) {
	// hold a free port
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := held.Addr().(*net.TCPAddr).Port
	if port+PortFallback > 65535 {
		t.Skip("port too high")
	}

	// from config/default: the next free port
	ls, got, err := listenAll([]string{"127.0.0.1"}, port, false)
	if err != nil {
		t.Fatal(err)
	}
	if got <= port || got > port+PortFallback || ls[0].Addr().String() != "127.0.0.1:"+strconv.Itoa(got) {
		t.Errorf("fell back to %d (held %d)", got, port)
	}
	for _, l := range ls {
		l.Close()
	}

	// explicit --port: a clear error
	_, _, err = listenAll([]string{"127.0.0.1"}, port, true)
	if err == nil || !strings.Contains(err.Error(), "port "+strconv.Itoa(port)+" is already in use") || !isAddrInUse(err) {
		t.Errorf("explicit taken port: %v", err)
	}

	// a free port is used as is
	free := got
	ls, got, err = listenAll([]string{"127.0.0.1"}, free, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != free {
		t.Errorf("free port %d moved to %d", free, got)
	}
	for _, l := range ls {
		l.Close()
	}
}

func TestLoggerAlwaysWritesFile(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	p := logFilePath(cfg, filepath.Join(dir, "config.json"))
	if p != filepath.Join(dir, "even-cc.log") {
		t.Fatalf("default log path %s", p)
	}
	for _, level := range []string{"info", "debug", "warn"} {
		file := filepath.Join(dir, level+".log")
		log, closer, err := newLogger(level, file)
		if err != nil {
			t.Fatal(err)
		}
		log.Debug("raw cli line")
		log.Info("prompt")
		closer.Close()
		b, _ := os.ReadFile(file)
		gotDebug, gotInfo := strings.Contains(string(b), "raw cli line"), strings.Contains(string(b), "prompt")
		if !gotInfo || gotDebug != (level == "debug") {
			t.Errorf("%s: file has debug=%v info=%v", level, gotDebug, gotInfo)
		}
	}
}
