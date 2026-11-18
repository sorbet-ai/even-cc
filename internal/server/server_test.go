package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"even-cc/internal/claude"
	"even-cc/internal/events"
	"even-cc/internal/hub"
	"even-cc/internal/info"
	"even-cc/internal/transcript"
)

const token = "TOKEN0123456789abcdef0123456789ab"
const fixtures = "../../testdata/even/http/"

type env struct {
	t   *testing.T
	srv *httptest.Server
	s   *Server
	hub *hub.Hub
	cwd string
	sid string // an existing transcript
}

func newEnv(t *testing.T) *env {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	claudeDir := t.TempDir()
	cwd := t.TempDir()
	store := transcript.NewStore(claudeDir)
	sid := "b9f3dc0c-7bdb-4174-bf79-cf29f9a53f30"
	dir := filepath.Join(claudeDir, "projects", transcript.ProjectDirName(cwd))
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(
		`{"type":"user","uuid":"u1","parentUuid":null,"cwd":`+string(mustJSON(cwd))+`,"message":{"role":"user","content":"hello there"}}`+"\n"+
			`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"hi"}]}}`+"\n"+
			`{"type":"ai-title","aiTitle":"EVEN"}`+"\n"), 0o644)
	h := hub.New(nil)
	mgr := claude.NewManager(claude.Options{ClaudePath: filepath.Join(t.TempDir(), "no-claude"), DefaultCwd: cwd, StatsInterval: -1}, claude.ClassicDisplay(), h, store, log)
	inf := info.New("", nil, store, "off", log)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	inf.Start(ctx)
	s := &Server{Token: token, Mgr: mgr, Hub: h, Store: store, Log: log, Version: "0.1.0", Name: "mule", Info: inf}
	e := &env{t: t, srv: httptest.NewServer(s), s: s, hub: h, cwd: cwd, sid: sid}
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) do(method, path, body string, hdr map[string]string) (int, http.Header, string) {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if hdr == nil {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func fixture(t *testing.T, name string) string {
	b, err := os.ReadFile(fixtures + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(b), "\n")
}

func expect(t *testing.T, gotStatus int, got string, wantStatus int, want string) {
	t.Helper()
	if gotStatus != wantStatus || strings.TrimRight(got, "\n") != want {
		t.Errorf("got %d %s\nwant %d %s", gotStatus, got, wantStatus, want)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	want := fixture(t, "error.auth.401.json")
	st, h, body := e.do("GET", "/api/sessions", "", map[string]string{})
	expect(t, st, body, 401, want)
	if h.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("content-type %s", h.Get("Content-Type"))
	}
	st, _, body = e.do("GET", "/api/sessions?token="+token, "", map[string]string{"Authorization": "Bearer wrong"})
	expect(t, st, body, 401, want)
	st, _, _ = e.do("GET", "/api/sessions?token="+token, "", map[string]string{"Authorization": "Basic xyz"})
	if st != 200 {
		t.Errorf("Basic + query token: %d", st)
	}
	st, _, _ = e.do("GET", "/api/sessions?token="+token+"&token="+token, "", map[string]string{})
	if st != 401 {
		t.Errorf("repeated token: %d", st)
	}
	st, _, body = e.do("GET", "/api/nonexistent", "", map[string]string{})
	expect(t, st, body, 401, want)
}

func TestStaticResponses(t *testing.T) {
	e := newEnv(t)
	st, h, body := e.do("GET", "/?token="+token+"&defaultProvider=claude", "", map[string]string{})
	expect(t, st, body, 404, fixture(t, "root.404.html"))
	if h.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("root content-type %s", h.Get("Content-Type"))
	}
	st, _, body = e.do("GET", "/api/nonexistent", "", nil)
	expect(t, st, body, 404, strings.Replace(fixture(t, "root.404.html"), "Cannot GET /", "Cannot GET /api/nonexistent", 1))

	cases := []struct {
		method, path, body string
		status             int
		file               string
	}{
		{"GET", "/api/sessions?provider=bogus", "", 400, "error.unsupported-provider.400.json"},
		{"POST", "/api/prompt", `{"text":"x","provider":"bogus"}`, 400, "error.unsupported-provider.400.json"},
		{"GET", "/api/events", "", 400, "error.events-missing-sessionId.400.json"},
		{"GET", "/api/sessions/nope/history?limit=10&provider=claude", "", 200, "history.unknown-session.200.json"},
		{"GET", "/api/messages?sessionId=nope", "", 200, "messages.unknown-session.200.json"},
		{"GET", "/api/status", "", 400, "error.missing-sessionId.400.json"},
		{"GET", "/api/status?sessionId=" + e.sid, "", 404, "error.session-not-found.404.json"},
		{"POST", "/api/prompt", `{"sessionId":"x"}`, 400, "error.missing-text.400.json"},
		{"POST", "/api/prompt", `{"sessionId":"00000000-0000-4000-8000-000000000000","text":"hi","provider":"claude"}`, 404, "error.prompt-session-not-found.404.json"},
		{"POST", "/api/permission-response", `{"decision":"allow"}`, 400, "error.missing-sessionId.400.json"},
		{"POST", "/api/permission-response", `{"sessionId":"` + e.sid + `","decision":"allow"}`, 404, "error.session-not-found.404.json"},
		{"POST", "/api/question-response", `{"sessionId":"` + e.sid + `","answer":"x"}`, 404, "error.session-not-found.404.json"},
		{"POST", "/api/interrupt", `{}`, 400, "error.missing-sessionId.400.json"},
		{"GET", "/api/sessions?provider=codex", "", 200, `{"sessions":[]}`},
	}
	for _, c := range cases {
		want := c.file
		if !strings.HasPrefix(want, "{") {
			want = fixture(t, c.file)
		}
		st, _, body := e.do(c.method, c.path, c.body, nil)
		if st != c.status || strings.TrimRight(body, "\n") != want {
			t.Errorf("%s %s: got %d %s\nwant %d %s", c.method, c.path, st, body, c.status, want)
		}
	}
}

func TestSessionsAndHistoryShapes(t *testing.T) {
	e := newEnv(t)
	st, _, body := e.do("GET", "/api/sessions?provider=claude", "", nil)
	if st != 200 {
		t.Fatal(st)
	}
	var got struct {
		Sessions []map[string]any `json:"sessions"`
	}
	_ = json.Unmarshal([]byte(body), &got)
	if len(got.Sessions) != 1 || got.Sessions[0]["title"] != "EVEN" || got.Sessions[0]["status"] != "idle" || got.Sessions[0]["cwd"] != e.cwd {
		t.Fatalf("sessions %s", body)
	}
	// key order identical to the live fixture
	fx := fixture(t, "sessions.claude.200.json")
	firstObj := func(s string) string { return s[strings.Index(s, "{\"id\""):strings.Index(s, "}")] }
	keys := func(obj string) []string {
		var ks []string
		for _, part := range strings.Split(obj, `,"`) {
			ks = append(ks, strings.Trim(strings.SplitN(part, `":`, 2)[0], `{"`))
		}
		return ks
	}
	if !reflect.DeepEqual(keys(firstObj(body)), keys(firstObj(fx))) {
		t.Errorf("keys %v vs %v", keys(firstObj(body)), keys(firstObj(fx)))
	}
	ts := got.Sessions[0]["timestamp"].(string)
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", ts); err != nil {
		t.Errorf("timestamp %s", ts)
	}
	st, _, body = e.do("GET", "/api/sessions/"+e.sid+"/history?limit=10&provider=claude", "", nil)
	if st != 200 || body != `{"history":[{"role":"user","text":"hello there"},{"role":"assistant","text":"hi"}]}` {
		t.Errorf("history %d %s", st, body)
	}
}

func TestPromptLifecycleWithoutClaude(t *testing.T) {
	e := newEnv(t)
	// claude binary is missing: prompt is still accepted (202), then error + idle
	st, _, body := e.do("POST", "/api/prompt", `{"sessionId":"`+e.sid+`","text":"hi","provider":"claude"}`, nil)
	if st != 202 || body != `{"ok":true,"sessionId":"`+e.sid+`","provider":"claude"}` {
		t.Fatalf("prompt %d %s", st, body)
	}
	deadline := time.Now().Add(3 * time.Second)
	var msgs string
	for time.Now().Before(deadline) {
		_, _, msgs = e.do("GET", "/api/messages?sessionId="+e.sid, "", nil)
		if strings.Contains(msgs, `"state":"idle","sessionId"`) && strings.Contains(msgs, `"idle"`) && strings.Contains(msgs, `"type":"error"`) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.HasPrefix(msgs, `{"messages":[{"id":1,"type":"user_prompt","text":"hi"}`) || !strings.Contains(msgs, `"type":"error"`) || !strings.HasSuffix(msgs, `"state":"idle","sessionId":"`+e.sid+`","provider":"claude"}`) {
		t.Errorf("messages %s", msgs)
	}
	st, _, body = e.do("GET", "/api/status?sessionId="+e.sid, "", nil)
	expect(t, st, body, 200, fixture(t, "status.200.json"))
	st, _, body = e.do("POST", "/api/permission-response", `{"sessionId":"`+e.sid+`","decision":"allow","provider":"claude"}`, nil)
	expect(t, st, body, 400, fixture(t, "error.permission-not-pending.400.json"))
	st, _, body = e.do("POST", "/api/question-response", `{"sessionId":"`+e.sid+`","answer":"x"}`, nil)
	expect(t, st, body, 200, fixture(t, "ok.200.json"))
	st, _, body = e.do("POST", "/api/interrupt", `{"sessionId":"`+e.sid+`"}`, nil)
	expect(t, st, body, 200, fixture(t, "ok.200.json"))
}

func TestSSEFramingAndReplay(t *testing.T) {
	e := newEnv(t)
	HeartbeatInterval = 50 * time.Millisecond
	defer func() { HeartbeatInterval = 15 * time.Second }()
	e.hub.Emit("s1", events.TextDelta{Type: "text_delta", Text: "a<b>& "})
	resp, err := http.Get(e.srv.URL + "/api/events?token=" + token + "&sessionId=s1&needReplay=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Errorf("headers %v", resp.Header)
	}
	r := bufio.NewReader(resp.Body)
	read := func(n int) string {
		var b bytes.Buffer
		for i := 0; i < n; i++ {
			l, err := r.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			b.WriteString(l)
		}
		return b.String()
	}
	if got := read(2); got != ":ok\n\n" {
		t.Fatalf("first frame %q", got)
	}
	if got := read(3); got != "id: 1\ndata: {\"type\":\"text_delta\",\"text\":\"a<b>& \"}\n\n" {
		t.Errorf("replay frame %q", got)
	}
	e.hub.Emit("s1", events.NewStatus("idle", "s1"))
	if got := read(3); got != `id: 2`+"\n"+`data: {"type":"status","state":"idle","sessionId":"s1","provider":"claude"}`+"\n\n" {
		t.Errorf("live frame %q", got)
	}
	if got := read(2); got != ":heartbeat\n\n" {
		t.Errorf("heartbeat %q", got)
	}
	// no replay without needReplay
	resp2, err := http.Get(e.srv.URL + "/api/events?sessionId=s1&token=" + token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	r2 := bufio.NewReader(resp2.Body)
	l1, _ := r2.ReadString('\n')
	l2, _ := r2.ReadString('\n')
	e.hub.Emit("s1", events.TextDelta{Type: "text_delta", Text: "x"})
	l3, _ := r2.ReadString('\n')
	if l1+l2 != ":ok\n\n" || l3 != "id: 3\n" {
		t.Errorf("no-replay stream %q %q", l1+l2, l3)
	}
}

func TestRingBufferKeepsActiveTurn(t *testing.T) {
	h := hub.New(nil)
	h.Emit("s", events.NewUserPrompt("p"))
	for i := 0; i < 600; i++ {
		h.Emit("s", events.TextDelta{Type: "text_delta", Text: "x"})
	}
	var all []map[string]any
	_ = json.Unmarshal(h.Messages("s", 0), &all)
	if len(all) != 601 || all[0]["type"] != "user_prompt" {
		t.Errorf("active turn trimmed: %d", len(all))
	}
	h.Emit("s", events.NewStatus("idle", "s"))
	h.Emit("s", events.TextDelta{Type: "text_delta", Text: "y"})
	_ = json.Unmarshal(h.Messages("s", 0), &all)
	if len(all) != 500 || all[0]["id"].(float64) != 104 {
		t.Errorf("after idle: %d first %v", len(all), all[0]["id"])
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestMachine(t *testing.T) {
	e := newEnv(t)
	st, _, body := e.do("GET", "/api/machine", "", nil)
	if st != 200 || !strings.Contains(body, `"name":"mule"`) || !strings.Contains(body, `"usage":null`) {
		t.Fatalf("%d %s", st, body)
	}
}
