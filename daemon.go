package main

// Background mode: `even-cc start` runs the server detached, prints the
// setup QR once it listens, and returns; `even-cc stop` and `even-cc status`
// find it through daemon.json next to the config.

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sorbet-ai/even-cc/internal/config"
	"github.com/sorbet-ai/even-cc/internal/netaddr"
)

// daemonEnv marks the detached server process.
const daemonEnv = "EVEN_CC_DAEMON"

// daemonState is what the detached server writes once it listens.
type daemonState struct {
	Pid       int       `json:"pid"`
	Port      int       `json:"port"`
	Name      string    `json:"name"`
	STT       bool      `json:"stt"`
	Terminal  string    `json:"terminal"`
	Log       string    `json:"log"`
	StartedAt time.Time `json:"startedAt"`
}

func daemonPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "daemon.json")
}

func readDaemon(configPath string) (*daemonState, error) {
	b, err := os.ReadFile(daemonPath(configPath))
	if err != nil {
		return nil, err
	}
	var st daemonState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// runningDaemon is the live background server, or nil (a stale file is removed).
func runningDaemon(configPath string) *daemonState {
	st, err := readDaemon(configPath)
	if err != nil {
		return nil
	}
	if !pidAlive(st.Pid) {
		_ = os.Remove(daemonPath(configPath))
		return nil
	}
	return st
}

// writeDaemonState records this (detached) server; the returned func removes it.
func writeDaemonState(configPath string, st daemonState) func() {
	p := daemonPath(configPath)
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "cannot write", p, err)
	}
	return func() {
		if cur, err := readDaemon(configPath); err == nil && cur.Pid == st.Pid {
			_ = os.Remove(p)
		}
	}
}

func startCmd(args []string) error {
	o := &serveOpts{}
	fs := serveFlags(o)
	if err := parseServe(fs, o, args); err != nil {
		return err
	}
	if st := runningDaemon(o.configPath); st != nil {
		fmt.Printf("\n  even-cc is already running in the background (pid %d, port %d).\n", st.Pid, st.Port)
		return printDaemonSetup(o, st)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	out := filepath.Join(filepath.Dir(o.configPath), "even-cc.out")
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	childArgs := []string{"serve", "--no-qr"} // the QR is printed here, once it listens
	for _, a := range args {
		if a != "--no-qr" && a != "-no-qr" {
			childArgs = append(childArgs, a)
		}
	}
	cmd := exec.Command(exe, childArgs...)
	cmd.Env = append(os.Environ(), daemonEnv+"=1")
	cmd.Stdout, cmd.Stderr = f, f
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd
	}
	if err := startDetached(cmd); err != nil {
		return fmt.Errorf("cannot start the background server: %w", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	// wait for it to listen (it writes daemon.json), or to fail
	deadline := time.Now().Add(20 * time.Second)
	for {
		if st, err := readDaemon(o.configPath); err == nil && st.Pid == pid {
			fmt.Printf("\n  even-cc is running in the background (pid %d).\n", pid)
			return printDaemonSetup(o, st)
		}
		select {
		case <-exited:
			b, _ := os.ReadFile(out)
			return fmt.Errorf("the background server exited:\n%s", tailLines(string(b), 15))
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the background server (pid %d) didn't start listening in 20 s; see %s", pid, out)
		}
	}
}

// printDaemonSetup prints the setup QR for the running server and how to stop it.
func printDaemonSetup(o *serveOpts, st *daemonState) error {
	cfg, err := loadConfig(o.configPath)
	if err != nil {
		return err
	}
	if o.bind != "" {
		cfg.Bind = o.bind
	}
	host, err := netaddr.Resolve(cfg.Bind)
	if err != nil {
		return err
	}
	printSetup(host, st.Port, cfg.Token, st.Name, st.STT, st.Terminal, o.noQR)
	fmt.Printf("  Stop it:  even-cc stop\n  Status:   even-cc status\n  Log:      %s\n\n", st.Log)
	return nil
}

func stopCmd(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	path := fs.String("config", config.DefaultPath(), "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st := runningDaemon(*path)
	if st == nil {
		fmt.Println("even-cc isn't running in the background.")
		return nil
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	// polite: the server closes its Claude processes and the mirror itself
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/shutdown", st.Port), nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	if resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req); err == nil {
		resp.Body.Close()
	}
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if !pidAlive(st.Pid) {
			_ = os.Remove(daemonPath(*path))
			fmt.Printf("Stopped even-cc (pid %d).\n", st.Pid)
			return nil
		}
	}
	if err := killPid(st.Pid); err != nil {
		return fmt.Errorf("even-cc (pid %d) didn't stop: %w", st.Pid, err)
	}
	_ = os.Remove(daemonPath(*path))
	fmt.Printf("Killed even-cc (pid %d): it didn't stop in 15 s.\n", st.Pid)
	return nil
}

func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	path := fs.String("config", config.DefaultPath(), "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st := runningDaemon(*path)
	if st == nil {
		fmt.Println("even-cc isn't running in the background. Start it: even-cc start")
		return nil
	}
	fmt.Printf("even-cc is running in the background: pid %d, port %d, up %s.\nLog: %s\nStop it: even-cc stop\n",
		st.Pid, st.Port, time.Since(st.StartedAt).Round(time.Second), st.Log)
	return nil
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
