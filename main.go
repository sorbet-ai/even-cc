// Command even-cc serves your Claude Code sessions to Faceclaw on Even
// Realities G2 glasses: the Claude Code app (sessions, live events, prompts,
// permission and question answers), local Whisper speech-to-text, and the
// Terminal app's mirror of the real Claude Code screen.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sorbet-ai/even-cc/internal/claude"
	"github.com/sorbet-ai/even-cc/internal/config"
	"github.com/sorbet-ai/even-cc/internal/hub"
	"github.com/sorbet-ai/even-cc/internal/info"
	"github.com/sorbet-ai/even-cc/internal/logfile"
	"github.com/sorbet-ai/even-cc/internal/mirror"
	"github.com/sorbet-ai/even-cc/internal/netaddr"
	"github.com/sorbet-ai/even-cc/internal/qrterm"
	"github.com/sorbet-ai/even-cc/internal/server"
	"github.com/sorbet-ai/even-cc/internal/transcript"
)

// Version is set at release build time (-ldflags "-X main.Version=v1.2.3").
var Version = "dev"

func usage() {
	fmt.Fprint(os.Stderr, `even-cc `+Version+` — Claude Code on Even G2 glasses, through Faceclaw

Usage:
  even-cc [serve] [flags]          start the server in this terminal (default)
  even-cc start [flags]            start it in the background and print the setup QR
  even-cc stop                     stop the background server
  even-cc status                   is the background server running?
  even-cc pair [--config path]     print the Faceclaw setup QR code
  even-cc config [--config path]   print the config (token masked)
  even-cc config <key>             print one key
  even-cc config <key> <value>     set a key, e.g. even-cc config display.tools hidden
  even-cc version

Serve flags:
`)
}

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "start":
		err = startCmd(args)
	case "stop":
		err = stopCmd(args)
	case "status":
		err = statusCmd(args)
	case "config":
		err = configCmd(args)
	case "pair":
		err = pairCmd(args)
	case "version", "--version":
		fmt.Println("even-cc", Version)
	case "help", "-h", "--help":
		usage()
		serveFlags(&serveOpts{}).PrintDefaults()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type serveOpts struct {
	configPath string
	port       int
	bind       string
	cwd        string
	name       string
	claudePath string
	logFile    string
	logLevel   string
	claudeDir  string
	noQR       bool
	portSet    bool // --port given explicitly
}

func serveFlags(o *serveOpts) *flag.FlagSet {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.StringVar(&o.configPath, "config", config.DefaultPath(), "config file")
	fs.IntVar(&o.port, "port", 0, "port (overrides config)")
	fs.StringVar(&o.bind, "bind", "", "lan | tailscale | interface:<name> | loopback (overrides config)")
	fs.StringVar(&o.cwd, "cwd", "", "directory for new sessions (overrides config)")
	fs.StringVar(&o.name, "name", "", "display name in the pairing URL (overrides config)")
	fs.StringVar(&o.claudePath, "claude", "", "path to the claude executable (overrides config)")
	fs.StringVar(&o.claudeDir, "claude-dir", "", "Claude Code config dir to read sessions from and run claude with (CLAUDE_CONFIG_DIR; overrides config)")
	fs.StringVar(&o.logFile, "log-file", "", "log file (overrides config; default even-cc.log next to the config)")
	fs.StringVar(&o.logLevel, "log-level", "", "info | debug (adds raw CLI and event traffic to the log file) | warn | error (quieter stderr) (overrides config)")
	fs.BoolVar(&o.noQR, "no-qr", false, "don't print the QR code")
	return fs
}

// loadConfig loads (or creates, with a fresh token) the config.
func loadConfig(path string) (config.Config, error) {
	cfg, created, err := config.Load(path)
	if err != nil {
		return cfg, err
	}
	if created {
		fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	}
	return cfg, nil
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// fanout sends records to several handlers.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r.Clone())
		}
	}
	return nil
}

func (f fanout) WithAttrs(a []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(a)
	}
	return out
}

func (f fanout) WithGroup(n string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(n)
	}
	return out
}

// logFilePath is the configured log file, or even-cc.log next to the config.
func logFilePath(cfg config.Config, configPath string) string {
	if cfg.Log.File != "" {
		return cfg.Log.File
	}
	return filepath.Join(filepath.Dir(configPath), "even-cc.log")
}

// newLogger always writes the log file (info, or debug with the raw CLI and
// event traffic) and stderr (info; warn/error when set, never debug).
func newLogger(level, file string) (*slog.Logger, io.Closer, error) {
	stderrLevel, fileLevel := parseLevel(level), slog.LevelInfo
	if stderrLevel < slog.LevelInfo {
		stderrLevel, fileLevel = slog.LevelInfo, stderrLevel
	}
	lf, err := logfile.Open(file, logfile.DefaultMaxBytes, logfile.DefaultKeep)
	if err != nil {
		return nil, nil, fmt.Errorf("log file: %w", err)
	}
	handlers := fanout{
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: stderrLevel}),
		slog.NewTextHandler(lf, &slog.HandlerOptions{Level: fileLevel}),
	}
	return slog.New(handlers), lf, nil
}

// crashOutput sends the runtime's report of an unrecovered panic or fatal
// error (normally stderr only) to <log file>.crash too, so a crash leaves
// evidence even when nobody kept the terminal.
func crashOutput(logPath string, log *slog.Logger) {
	path := strings.TrimSuffix(logPath, ".log") + ".crash.log"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Warn("crash output file", "err", err)
		return
	}
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		log.Warn("crash output file", "err", err)
	}
	f.Close() // SetCrashOutput keeps its own copy
}

func maskToken(t string) string {
	if len(t) <= 12 {
		return "…"
	}
	return t[:8] + "..." + t[len(t)-4:]
}

// printSetup prints the QR code that adds this PC to Faceclaw: the phone
// camera opens even-cc's setup page, which hands Faceclaw the details.
func printSetup(host netaddr.Host, port int, token, name string, stt bool, terminal string, noQR bool) {
	if host.Address == "" || host.Address == "127.0.0.1" {
		fmt.Println("\n  Loopback only: the phone can't reach this PC (use --bind lan|tailscale|interface:<name>).")
		return
	}
	u := server.SetupURL(host.Address, port, token, name, stt, terminal)
	fmt.Println()
	fmt.Println("  Add this machine in Faceclaw: Claude Code > Settings > Machines > Add machine:")
	fmt.Println("  " + u)
	if !noQR {
		if qr, err := qrterm.Render(u); err == nil {
			fmt.Println()
			fmt.Print(qr)
		}
	}
	fmt.Println()
}

// machineName names this PC in Faceclaw: the config's name, else the host
// name, marked when it is WSL (which shares the Windows host name).
func machineName(configured string) string {
	if configured != "" {
		return configured
	}
	name, _ := os.Hostname()
	if b, err := os.ReadFile("/proc/version"); err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft") {
		name += " (WSL)"
	}
	return name
}

func parseServe(fs *flag.FlagSet, o *serveOpts, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			o.portSet = true
		}
	})
	return nil
}

func serve(args []string) error {
	o := &serveOpts{}
	fs := serveFlags(o)
	if err := parseServe(fs, o, args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Args()[0])
	}
	return runServer(o)
}

// isAddrInUse reports a port already taken (EADDRINUSE; on Windows also
// WSAEADDRINUSE and WSAEACCES, which an excluded or WSL-shared port gives).
func isAddrInUse(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.EADDRINUSE || errno == 10048 || errno == 10013
	}
	return false
}

// PortFallback is how many ports after a taken configured port are tried.
const PortFallback = 20

// listenAll listens on port at every address. A port from the config or
// the default that is taken falls forward to the next free one (up to
// +PortFallback); an explicit --port must be free.
func listenAll(addrs []string, port int, explicit bool) ([]net.Listener, int, error) {
	try := func(p int) ([]net.Listener, error) {
		var ls []net.Listener
		for _, a := range addrs {
			l, err := net.Listen("tcp", net.JoinHostPort(a, strconv.Itoa(p)))
			if err != nil {
				for _, x := range ls {
					x.Close()
				}
				return nil, fmt.Errorf("listen %s:%d: %w", a, p, err)
			}
			ls = append(ls, l)
		}
		return ls, nil
	}
	ls, err := try(port)
	if err == nil {
		return ls, port, nil
	}
	if !isAddrInUse(err) {
		return nil, 0, err
	}
	if explicit {
		return nil, 0, fmt.Errorf("port %d is already in use (another even-cc or even-terminal?); pick another --port, or leave --port out to use the next free one: %w", port, err)
	}
	for p := port + 1; p <= port+PortFallback && p <= 65535; p++ {
		ls, err := try(p)
		if err == nil {
			return ls, p, nil
		}
		if !isAddrInUse(err) {
			return nil, 0, err
		}
	}
	return nil, 0, fmt.Errorf("ports %d-%d are all in use; set one with --port", port, port+PortFallback)
}

func runServer(o *serveOpts) error {
	cfg, err := loadConfig(o.configPath)
	if err != nil {
		return err
	}
	if o.port != 0 {
		cfg.Port = o.port
	}
	if o.bind != "" {
		cfg.Bind = o.bind
	}
	if o.cwd != "" {
		cfg.Cwd = o.cwd
	}
	if o.name != "" {
		cfg.Name = o.name
	}
	if o.claudePath != "" {
		cfg.Claude.Path = o.claudePath
	}
	if o.claudeDir != "" {
		cfg.ClaudeDir = o.claudeDir
	}
	if o.logLevel != "" {
		cfg.Log.Level = o.logLevel
	}
	if o.logFile != "" {
		cfg.Log.File = o.logFile
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	logPath := logFilePath(cfg, o.configPath)
	log, closer, err := newLogger(cfg.Log.Level, logPath)
	if err != nil {
		return err
	}
	defer closer.Close()
	crashOutput(logPath, log)
	log.Info("even-cc starting", "version", Version, "pid", os.Getpid(), "config", o.configPath, "log", logPath, "logLevel", cfg.Log.Level)

	claudePath, err := claude.FindClaude(cfg.Claude.Path)
	if err != nil {
		return err
	}
	host, err := netaddr.Resolve(cfg.Bind)
	if err != nil {
		return err
	}
	if cfg.Bind == "lan" && host.Address == "" {
		log.Warn("no private LAN IPv4 address found; listening on 127.0.0.1 only (use --bind interface:<name>)")
	}

	addrs := []string{"127.0.0.1"}
	if host.Address != "" && host.Address != "127.0.0.1" {
		addrs = append([]string{host.Address}, addrs...)
	}
	listeners, port, err := listenAll(addrs, cfg.Port, o.portSet)
	if err != nil {
		return err
	}
	if port != cfg.Port {
		log.Warn("configured port is in use; using the next free port", "configured", cfg.Port, "port", port)
		fmt.Fprintf(os.Stderr, "\n  WARNING: port %d is in use, so even-cc is on port %d. A phone paired with :%d won't find it;\n  scan the new QR below, or stop whatever holds %d.\n", cfg.Port, port, cfg.Port, cfg.Port)
		cfg.Port = port
	}
	closeListeners := func() {
		for _, l := range listeners {
			l.Close()
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	env := os.Environ()
	claudeDir := transcript.ClaudeDir()
	if cfg.ClaudeDir != "" {
		claudeDir = cfg.ClaudeDir
		env = append(env, "CLAUDE_CONFIG_DIR="+cfg.ClaudeDir)
	}
	h := hub.New(log)
	store := transcript.NewStore(claudeDir)
	inf := info.New(claudePath, env, log)
	inf.Start(ctx)

	minutes := func(m float64) time.Duration { return time.Duration(m * float64(time.Minute)) }
	glassesPrompt := ""
	if cfg.Claude.GlassesPrompt {
		// A file, not an argument: a long multi-line argument is fragile on Windows.
		glassesPrompt = filepath.Join(filepath.Dir(o.configPath), "glasses-prompt.md")
		if err := os.WriteFile(glassesPrompt, []byte(claude.GlassesPrompt()), 0o600); err != nil {
			log.Warn("cannot write the glasses prompt; claude runs without it", "path", glassesPrompt, "err", err)
			glassesPrompt = ""
		}
	}
	mgr := claude.NewManager(claude.Options{
		ClaudePath:             claudePath,
		SettingSources:         cfg.Claude.SettingSources,
		PermissionMode:         cfg.Claude.PermissionMode,
		Model:                  cfg.Claude.Model,
		AllowedTools:           cfg.Claude.AllowedTools,
		MaxTurns:               cfg.Claude.MaxTurns,
		ExtraArgs:              cfg.Claude.ExtraArgs,
		AppendSystemPromptFile: glassesPrompt,
		IdleTimeout:            minutes(cfg.Claude.IdleTimeoutMinutes),
		PermissionTimeout:      minutes(cfg.Timeouts.PermissionMinutes),
		QuestionTimeout:        minutes(cfg.Timeouts.QuestionMinutes),
		DefaultCwd:             cfg.Cwd,
		Env:                    env,
		ClaudeDir:              claudeDir,
	}, claude.DisplayFromConfig(cfg.Display), h, store, log)
	mgr.SetInitializeHook(inf.FromInitialize)

	srv := &server.Server{Token: cfg.Token, Mgr: mgr, Hub: h, Store: store, Log: log, Version: Version, Name: machineName(cfg.Name), Info: inf}
	if cfg.SttURL != "" {
		stt, err := server.NewSTTProxy(cfg.SttURL)
		if err != nil {
			return fmt.Errorf("sttUrl %q: %w", cfg.SttURL, err)
		}
		srv.STT = stt
	}

	fmt.Printf("\n  even-cc %s  ·  Claude Code at %s\n", Version, claudePath)
	fmt.Printf("  Local:   http://localhost:%d\n", cfg.Port)
	if host.Address != "" && host.Address != "127.0.0.1" {
		fmt.Printf("  %-8s http://%s:%d\n", host.Label+":", host.Address, cfg.Port)
	}
	fmt.Printf("  Name:    %s\n", srv.Name)
	fmt.Printf("  Token:   %s\n", maskToken(cfg.Token))
	fmt.Printf("  CWD:     %s\n", cfg.Cwd)
	if cfg.ClaudeDir != "" {
		fmt.Printf("  Claude:  %s\n", cfg.ClaudeDir)
	}
	fmt.Printf("  Config:  %s\n", o.configPath)
	fmt.Printf("  Log:     %s\n", logPath)
	if cfg.SttURL != "" {
		addr := "127.0.0.1"
		if host.Address != "" {
			addr = host.Address
		}
		fmt.Printf("  Speech:  http://%s:%d%s/v1 -> %s (key: the token)\n", addr, cfg.Port, server.STTPrefix, cfg.SttURL)
	}
	mirrorSrv, mirrorHTTP, mirrorConn := startMirror(cfg, host, addrs, claudePath, env, log)
	srv.Mirror = mirrorConn
	printSetup(host, cfg.Port, cfg.Token, srv.Name, srv.STT != nil, srv.Mirror, o.noQR)
	srv.Shutdown = stop

	// A WebSocket is hijacked, so these don't apply to it (ws.go bounds its writes).
	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 2 * time.Minute, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	var wg sync.WaitGroup
	errc := make(chan error, len(listeners))
	for _, l := range listeners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := httpSrv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	log.Info("listening", "addrs", strings.Join(addrs, ","), "port", cfg.Port)
	if os.Getenv(daemonEnv) == "1" { // started by `even-cc start`: tell it we're up
		defer writeDaemonState(o.configPath, daemonState{Pid: os.Getpid(), Port: cfg.Port, Name: srv.Name,
			STT: srv.STT != nil, Terminal: srv.Mirror, Log: logPath, StartedAt: time.Now()})()
	}
	if len(cfg.Token) < 16 {
		log.Warn("the pairing token is short and could be guessed by anyone on the network; set a longer one with: even-cc config token <32 random hex chars>", "length", len(cfg.Token))
	}

	select {
	case <-ctx.Done():
	case err := <-errc:
		log.Error("server failed", "err", err)
	}
	stop()
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = httpSrv.Shutdown(sctx)
	cancel()
	_ = httpSrv.Close()
	closeListeners()
	if mirrorHTTP != nil {
		_ = mirrorHTTP.Close()
		mirrorSrv.Close()
	}
	mgr.Shutdown()
	wg.Wait()
	return nil
}

// startMirror serves interactive Claude Code to Faceclaw's Terminal app on
// cfg.Mirror.Port. A failure is logged, not fatal: the phone app keeps working.
func startMirror(cfg config.Config, host netaddr.Host, addrs []string, claudePath string, env []string, log *slog.Logger) (*mirror.Server, *http.Server, string) {
	if cfg.Mirror.Port == 0 {
		return nil, nil, ""
	}
	name := machineName(cfg.Name)
	childEnv := append(claude.ScrubEnv(env), "TERM=xterm-256color", "COLORTERM=truecolor")
	ms := &mirror.Server{Token: cfg.Token, Name: name, Log: log, Launch: func(preset, cwd string) ([]string, string, []string, error) {
		argv := []string{claudePath}
		switch preset {
		case "shell", "claude":
		case "continue":
			argv = append(argv, "--continue")
		default:
			return nil, "", nil, fmt.Errorf("unknown preset %q (shell, claude, continue)", preset)
		}
		argv = append(argv, cfg.Mirror.Args...)
		dir := cfg.Cwd
		if cwd != "" {
			if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
				return nil, "", nil, fmt.Errorf("no such directory: %s", cwd)
			}
			dir = cwd
		}
		return argv, dir, childEnv, nil
	}}
	ls, _, err := listenAll(addrs, cfg.Mirror.Port, true)
	if err != nil {
		log.Warn("Faceclaw terminal mirror is off: cannot listen", "port", cfg.Mirror.Port, "err", err)
		fmt.Fprintf(os.Stderr, "  Faceclaw terminal: off (port %d: %v)\n", cfg.Mirror.Port, err)
		return nil, nil, ""
	}
	// websockets live long: only the header read is bounded
	hs := &http.Server{Handler: ms, ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	for _, l := range ls {
		go func() {
			if err := hs.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("mirror listener failed", "err", err)
			}
		}()
	}
	addr := "127.0.0.1"
	if host.Address != "" {
		addr = host.Address
	}
	conn := server.MirrorConn(cfg.Token, addr, cfg.Mirror.Port)
	fmt.Printf("  Terminal: %s (added to Faceclaw with the setup QR)\n", conn)
	log.Info("mirror listening", "addrs", strings.Join(addrs, ","), "port", cfg.Mirror.Port)
	return ms, hs, conn
}

func configCmd(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	path := fs.String("config", config.DefaultPath(), "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	switch len(rest) {
	case 0:
		shown := cfg
		shown.Token = maskToken(cfg.Token)
		b, _ := jsonIndent(shown)
		fmt.Println(string(b))
		return nil
	case 1:
		v, err := config.Get(cfg, rest[0])
		if err != nil {
			return err
		}
		fmt.Println(v)
		return nil
	case 2:
		next, err := config.Set(cfg, rest[0], rest[1])
		if err != nil {
			return err
		}
		if err := config.Save(*path, next); err != nil {
			return err
		}
		v, _ := config.Get(next, rest[0])
		if rest[0] == "token" {
			v = maskToken(v)
		}
		fmt.Printf("%s = %s\n", rest[0], v)
		return nil
	}
	return errors.New("usage: even-cc config [<key> [<value>]]")
}

func pairCmd(args []string) error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	path := fs.String("config", config.DefaultPath(), "config file")
	bind := fs.String("bind", "", "override bind mode")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	if *bind != "" {
		cfg.Bind = *bind
	}
	host, err := netaddr.Resolve(cfg.Bind)
	if err != nil {
		return err
	}
	terminal := ""
	if cfg.Mirror.Port != 0 && host.Address != "" {
		terminal = server.MirrorConn(cfg.Token, host.Address, cfg.Mirror.Port)
	}
	printSetup(host, cfg.Port, cfg.Token, machineName(cfg.Name), cfg.SttURL != "", terminal, false)
	return nil
}

func jsonIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
