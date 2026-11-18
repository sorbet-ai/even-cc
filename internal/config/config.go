// Package config loads and edits ~/.even-cc/config.json.
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Claude struct {
	// Path to the claude executable; empty = find claude / claude.exe on PATH.
	Path string `json:"path"`
	// Model passed as --model on launch; empty = account/settings default.
	Model string `json:"model"`
	// PermissionMode passed as --permission-mode on launch; "" = none, so
	// claude uses permissions.defaultMode from the user's settings.
	PermissionMode string `json:"permissionMode"`
	// AllowedTools replaces the default auto-allowed tool list when non-nil.
	AllowedTools []string `json:"allowedTools,omitempty"`
	// SettingSources is passed as --setting-sources.
	SettingSources string `json:"settingSources"`
	// MaxTurns per prompt (--max-turns); 0 = no limit.
	MaxTurns int `json:"maxTurns"`
	// IdleTimeoutMinutes closes an idle warm process; 0 = never.
	IdleTimeoutMinutes float64 `json:"idleTimeoutMinutes"`
	// ExtraArgs are appended to the claude command line.
	ExtraArgs []string `json:"extraArgs,omitempty"`
	// GlassesPrompt appends the glasses writing rules (the /even skill) to
	// claude's system prompt for the sessions even-cc runs.
	GlassesPrompt bool `json:"glassesPrompt"`
}

type Timeouts struct {
	PermissionMinutes float64 `json:"permissionMinutes"`
	QuestionMinutes   float64 `json:"questionMinutes"`
}

type Display struct {
	// Tools: full | collapsed | hidden.
	Tools string `json:"tools"`
	// Thinking sends think_start/think_end status events.
	Thinking bool `json:"thinking"`
	// Summaries: rich | classic (even-terminal's exact strings).
	Summaries string `json:"summaries"`
	// MaxOutputBytes caps tool_end detail.output; 0 = unlimited.
	MaxOutputBytes int `json:"maxOutputBytes"`
	// MaxInputChars caps each string in tool_end detail.input; 0 = unlimited.
	MaxInputChars int `json:"maxInputChars"`
	// ShortOptions uses short permission labels (Yes / Always (project) / No).
	ShortOptions bool `json:"shortOptions"`
	// SubagentTools sends tool_end events for tools run inside subagents.
	SubagentTools bool `json:"subagentTools"`
	// SubagentProgress shows one updating line per subagent (Agent tool).
	SubagentProgress bool `json:"subagentProgress"`
	// SubagentProgressSeconds is the minimum gap between progress updates of
	// one subagent line; 0 = only the start and finish lines.
	SubagentProgressSeconds int `json:"subagentProgressSeconds"`
	// Footer appends "— 2m10s · 4 tools · ctx 62%" to each reply.
	Footer bool `json:"footer"`
	// AgentProgressWhileIdle sends background subagent updates between turns
	// (the app then shows the session busy); off, they wait for the next turn.
	AgentProgressWhileIdle bool `json:"agentProgressWhileIdle"`
}

type Log struct {
	// Level: info (default) writes the log file and stderr; debug adds the
	// raw CLI and event traffic to the file; warn | error quiet stderr only.
	Level string `json:"level"`
	// File is the log file; "" = even-cc.log next to the config file. It is
	// always written, capped at a few MB with 2 old files kept.
	File string `json:"file"`
	// DebugFile is deprecated: a non-empty value means File=DebugFile and
	// Level=debug. It is dropped when the config is saved.
	DebugFile string `json:"debugFile,omitempty"`
}

type Config struct {
	Version int    `json:"version"`
	Port    int    `json:"port"`
	Token   string `json:"token"`
	Bind    string `json:"bind"` // lan | tailscale | interface:<name> | loopback
	Name    string `json:"name"`
	Cwd     string `json:"cwd"`
	// ClaudeDir is Claude Code's config dir (sessions under <dir>/projects);
	// "" = $CLAUDE_CONFIG_DIR or ~/.claude. When set, launched claude
	// processes get CLAUDE_CONFIG_DIR=<dir>.
	ClaudeDir string   `json:"claudeDir"`
	Claude    Claude   `json:"claude"`
	Timeouts  Timeouts `json:"timeouts"`
	Display   Display  `json:"display"`
	Log       Log      `json:"log"`
	Mirror    Mirror   `json:"mirror"`
	// SttURL is a local OpenAI-compatible Whisper server that even-cc forwards
	// /api/stt/* to (token-protected), so the phone can reach it; "" = off.
	SttURL string `json:"sttUrl"`
}

// Mirror serves interactive Claude Code sessions to Faceclaw's Terminal app
// (the g2mirror protocol).
type Mirror struct {
	// Port for g2mirror:// connections (8737 lets the connection string omit
	// it); 0 turns the mirror off.
	Port int `json:"port"`
	// Args are extra arguments for the claude it launches.
	Args []string `json:"args"`
}

// Default returns the defaults (token empty).
func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Version: 1,
		Port:    3456,
		Bind:    "lan",
		Cwd:     home,
		Claude: Claude{
			PermissionMode: "",
			SettingSources: "user,project,local",
			GlassesPrompt:  true,
		},
		Timeouts: Timeouts{PermissionMinutes: 10, QuestionMinutes: 10},
		Mirror:   Mirror{Port: 8737, Args: []string{}},
		Display: Display{
			Tools:          "collapsed",
			Thinking:       true,
			Summaries:      "rich",
			MaxOutputBytes: 1536,
			MaxInputChars:  300,
			ShortOptions:   true,
			SubagentTools:  false,

			SubagentProgress:        true,
			SubagentProgressSeconds: 15,
			Footer:                  true,
		},
		Log: Log{Level: "info"},
	}
}

// DefaultPath is ~/.even-cc/config.json.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".even-cc", "config.json")
	}
	return filepath.Join(home, ".even-cc", "config.json")
}

// NewToken returns 16 random bytes as 32 hex chars (even-terminal's format).
func NewToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Validate checks enumerations and ranges.
func (c *Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("version must be 1")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be 1-65535")
	}
	if c.Mirror.Port < 0 || c.Mirror.Port > 65535 {
		return fmt.Errorf("mirror.port must be 0 (off) or 1-65535")
	}
	if c.Mirror.Port != 0 && c.Mirror.Port == c.Port {
		return fmt.Errorf("mirror.port must differ from port")
	}
	switch {
	case c.Bind == "lan", c.Bind == "tailscale", c.Bind == "loopback":
	case strings.HasPrefix(c.Bind, "interface:") && len(c.Bind) > len("interface:"):
	default:
		return fmt.Errorf("bind must be lan | tailscale | interface:<name> | loopback")
	}
	switch c.Display.Tools {
	case "full", "collapsed", "hidden":
	default:
		return fmt.Errorf("display.tools must be full | collapsed | hidden")
	}
	switch c.Display.Summaries {
	case "rich", "classic":
	default:
		return fmt.Errorf("display.summaries must be rich | classic")
	}
	switch c.Claude.PermissionMode {
	case "", "default", "acceptEdits", "plan", "auto", "dontAsk", "bypassPermissions":
	default:
		return fmt.Errorf(`claude.permissionMode must be "" (from your Claude settings) | default | acceptEdits | plan | auto | dontAsk | bypassPermissions`)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level must be debug | info | warn | error")
	}
	if c.Display.SubagentProgressSeconds < 0 {
		return fmt.Errorf("display.subagentProgressSeconds must be >= 0")
	}
	if c.Log.DebugFile != "" { // deprecated key
		if c.Log.File == "" {
			c.Log.File = c.Log.DebugFile
		}
		c.Log.Level = "debug"
		c.Log.DebugFile = ""
	}
	if c.Timeouts.PermissionMinutes <= 0 || c.Timeouts.QuestionMinutes <= 0 {
		return fmt.Errorf("timeouts must be > 0")
	}
	return nil
}

// decode unmarshals JSON on top of defaults. A config file may hold keys of
// removed features (hub/join, talk while busy, ...): Load ignores unknown
// keys, and they drop out the next time the file is saved. Set is strict.
func decode(b []byte, strict bool) (Config, error) {
	c := Default()
	dec := json.NewDecoder(bytes.NewReader(b))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(&c); err != nil {
		return c, err
	}
	return c, c.Validate()
}

// Load reads path. If it doesn't exist, a config with a fresh token is
// created (seeded from importFrom, an even-terminal config, when given).
// created reports whether the file was written.
func Load(path, importFrom string) (cfg Config, created bool, err error) {
	b, err := os.ReadFile(path)
	if err == nil {
		cfg, err = decode(b, false)
		if err != nil {
			return cfg, false, fmt.Errorf("%s: %w", path, err)
		}
		if cfg.Token == "" {
			cfg.Token = NewToken()
			return cfg, true, Save(path, cfg)
		}
		return cfg, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return cfg, false, err
	}
	cfg = Default()
	if importFrom != "" {
		importEvenTerminal(&cfg, importFrom)
	}
	if cfg.Token == "" {
		cfg.Token = NewToken()
	}
	return cfg, true, Save(path, cfg)
}

// importEvenTerminal copies token/port/cwd/name from an even-terminal config
// so an already-paired phone keeps working.
func importEvenTerminal(cfg *Config, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var et struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
		Cwd   string `json:"cwd"`
		Name  string `json:"name"`
	}
	if json.Unmarshal(b, &et) != nil {
		return
	}
	if et.Token != "" {
		cfg.Token = et.Token
	}
	if et.Port > 0 && et.Port <= 65535 {
		cfg.Port = et.Port
	}
	if et.Cwd != "" {
		cfg.Cwd = et.Cwd
	}
	cfg.Name = et.Name
}

// Save writes cfg with mode 0600.
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Set assigns a dotted key (e.g. "display.tools") from its string form.
// Values that parse as JSON (numbers, booleans, arrays) are used as JSON;
// anything else is a string.
func Set(cfg Config, key, value string) (Config, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return cfg, err
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return cfg, err
	}
	parts := strings.Split(key, ".")
	node := root
	for _, p := range parts[:len(parts)-1] {
		child, ok := node[p].(map[string]any)
		if !ok {
			return cfg, fmt.Errorf("unknown key %q", key)
		}
		node = child
	}
	last := parts[len(parts)-1]
	old, ok := node[last]
	if !ok {
		// optional keys omitted when empty
		if !(key == "claude.allowedTools" || key == "claude.extraArgs") {
			return cfg, fmt.Errorf("unknown key %q", key)
		}
	}
	var v any
	if _, isStr := old.(string); isStr {
		v = value
		if value == `""` { // shells make an empty argument awkward to type
			v = ""
		}
	} else if err := json.Unmarshal([]byte(value), &v); err != nil {
		v = value
	}
	if value == "null" {
		v = nil
	}
	node[last] = v
	nb, _ := json.Marshal(root)
	out, err := decode(nb, true)
	if err != nil {
		return cfg, fmt.Errorf("%s=%s: %w", key, value, err)
	}
	return out, nil
}

// Get returns the JSON form of a dotted key.
func Get(cfg Config, key string) (string, error) {
	b, _ := json.Marshal(cfg)
	var node any
	_ = json.Unmarshal(b, &node)
	for _, p := range strings.Split(key, ".") {
		m, ok := node.(map[string]any)
		if !ok {
			return "", fmt.Errorf("unknown key %q", key)
		}
		node, ok = m[p]
		if !ok {
			return "", fmt.Errorf("unknown key %q", key)
		}
	}
	if s, ok := node.(string); ok {
		return s, nil
	}
	if f, ok := node.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	}
	out, _ := json.Marshal(node)
	return string(out), nil
}
