package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadCreateAndSet(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "config.json")
	cfg, created, err := Load(p)
	if err != nil || !created || len(cfg.Token) != 32 || cfg.Port != 3456 {
		t.Fatalf("%v %v %+v", err, created, cfg)
	}
	// Windows has no Unix modes (the file inherits the profile dir's ACL).
	if st, _ := os.Stat(p); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	cfg, err = Set(cfg, "display.tools", "hidden")
	if err != nil || cfg.Display.Tools != "hidden" {
		t.Fatal(err)
	}
	if cfg.Claude.PermissionMode != "" {
		t.Errorf("default permission mode %q, want \"\" (the user's settings)", cfg.Claude.PermissionMode)
	}
	if cfg, err = Set(cfg, "claude.permissionMode", "auto"); err != nil || cfg.Claude.PermissionMode != "auto" {
		t.Fatal(err)
	}
	if cfg, err = Set(cfg, "claude.permissionMode", `""`); err != nil || cfg.Claude.PermissionMode != "" {
		t.Fatalf("typed \"\" not taken as empty: %v %q", err, cfg.Claude.PermissionMode)
	}
	if cfg, err = Set(cfg, "port", "3458"); err != nil || cfg.Port != 3458 {
		t.Fatal(err)
	}
	if cfg, err = Set(cfg, "display.thinking", "false"); err != nil || cfg.Display.Thinking {
		t.Fatal(err)
	}
	if cfg, err = Set(cfg, "claude.allowedTools", `["Read","Grep"]`); err != nil || len(cfg.Claude.AllowedTools) != 2 {
		t.Fatal(err)
	}
	if _, err := Set(cfg, "display.tools", "bogus"); err == nil {
		t.Error("invalid value accepted")
	}
	if _, err := Set(cfg, "nope", "1"); err == nil {
		t.Error("unknown key accepted")
	}
	if _, err := Set(cfg, "bind", "interface:eth0"); err != nil {
		t.Error(err)
	}
	if v, _ := Get(cfg, "timeouts.permissionMinutes"); v != "10" {
		t.Errorf("get %s", v)
	}
	_ = os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{"version":1,"port":0}`), 0o600)
	if _, _, err := Load(filepath.Join(dir, "bad.json")); err == nil {
		t.Error("invalid value in file accepted")
	}
}

// Keys of removed features (hub/join mode, talk while busy, log.debugFile)
// are ignored,
// not settable or readable, and drop out on save.
func TestRemovedKeysIgnored(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	_ = os.WriteFile(p, []byte(`{"version":1,"port":3456,"token":"0123456789abcdef0123456789abcdef","claudeDir":"/c",
"hub":{"enabled":true,"joinCode":"K7Q4MD","tagLocal":false,"members":[{"name":"win","credentialHash":"ab","joinedAt":"x"}]},
"join":{"hub":"http://h:3456","credential":"cred","name":"home"},"allowCors":false,
"display":{"tools":"hidden","talkWhileBusy":true,"talkWhileBusyAfterSeconds":3},"log":{"level":"info","debugFile":"/x/d.log"}}`), 0o600)
	cfg, wrote, err := Load(p)
	if err != nil || wrote || cfg.Token != "0123456789abcdef0123456789abcdef" || cfg.ClaudeDir != "/c" || cfg.Display.Tools != "hidden" || cfg.Log.File != "" || cfg.Log.Level != "info" {
		t.Fatalf("%v %v %+v", err, wrote, cfg)
	}
	if _, err := Set(cfg, "hub.joinCode", "K7Q4MD"); err == nil {
		t.Error("hub.joinCode accepted")
	}
	if _, err := Set(cfg, "display.talkWhileBusy", "true"); err == nil {
		t.Error("display.talkWhileBusy accepted")
	}
	if _, err := Get(cfg, "join.hub"); err == nil {
		t.Error("join.hub readable")
	}
	if err := Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), `"hub"`) || strings.Contains(string(b), `"join"`) || strings.Contains(string(b), "talkWhileBusy") || strings.Contains(string(b), "debugFile") {
		t.Errorf("removed keys saved: %s", b)
	}
}
