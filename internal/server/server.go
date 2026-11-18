// Package server implements even-cc's HTTP and WebSocket API for Faceclaw's
// Claude Code app (based on even-terminal's, docs/even-protocol.md §2–§3).
package server

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"even-cc/internal/claude"
	"even-cc/internal/guard"
	"even-cc/internal/hub"
	"even-cc/internal/info"
	"even-cc/internal/jsonx"
	"even-cc/internal/transcript"
)

// maxBody caps request bodies.
const maxBody = 10 << 20

// maxHistory caps /history?limit=.
const maxHistory = 200

// Auth failures are logged at most authLogBurst times per authLogWindow.
const (
	authLogBurst  = 10
	authLogWindow = time.Minute
)

var started = time.Now()

type Server struct {
	Token   string
	Mgr     *claude.Manager
	Hub     *hub.Hub
	Store   *transcript.Store
	Log     *slog.Logger
	Version string
	// STT forwards /api/stt/* to a local Whisper server; nil = off.
	STT http.Handler
	// Name identifies this machine in Faceclaw (several PCs can be added).
	Name string
	// Mirror is the g2mirror:// connection for Faceclaw's Terminal app, or "".
	Mirror string
	// Info knows the Claude account this machine runs as; nil = unknown.
	Info *info.Info
	// Shutdown stops the server (POST /api/shutdown from this machine, for
	// `even-cc stop`); nil = not offered.
	Shutdown func()

	authMu         sync.Mutex
	authWindow     time.Time
	authLogged     int
	authSuppressed int
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var b []byte
	if raw, ok := v.([]byte); ok {
		b = raw
	} else {
		b = jsonx.Marshal(v)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

type errBody struct {
	Error string `json:"error"`
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errBody{Error: msg})
}

func notFound(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotFound, "Cannot "+r.Method+" "+r.URL.EscapedPath())
}

func redact(u *url.URL) string {
	q := u.Query()
	if q.Has("token") {
		q.Set("token", "REDACTED")
		return u.Path + "?" + q.Encode()
	}
	return u.RequestURI()
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool // headers sent
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Hijack lets WebSocket upgrades through (the library checks for
// http.Hijacker directly rather than unwrapping).
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	s.wrote = true
	s.status = http.StatusSwitchingProtocols
	return http.NewResponseController(s.ResponseWriter).Hijack()
}

// ServeHTTP routes like even-terminal's Express app.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: 200}
	// A panic in a handler is logged with its stack (net/http would also
	// recover, but only print to stderr); the server keeps serving.
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		if v == http.ErrAbortHandler {
			panic(v)
		}
		guard.Report(s.Log, "http "+r.Method+" "+r.URL.Path, v)
		if !rec.wrote {
			writeErr(rec, 500, "internal error (see the even-cc log)")
		}
	}()
	defer func() {
		if r.URL.Path == "/api/ws" {
			return
		}
		// Failed requests are logged at info (401s are logged by the auth
		// check); successful ones only at debug: the app polls every 10 s.
		lvl := slog.LevelDebug
		if rec.status >= 400 && rec.status != http.StatusUnauthorized {
			lvl = slog.LevelInfo
		}
		s.Log.Log(r.Context(), lvl, "http", "method", r.Method, "url", redact(r.URL), "status", rec.status, "ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
	}()
	w = rec
	path := r.URL.Path
	if r.Method == http.MethodGet && path == SetupPath {
		s.setup(w, r)
		return
	}
	if path != "/api" && !strings.HasPrefix(path, "/api/") {
		notFound(w, r)
		return
	}
	if !s.authorized(r) {
		s.logAuthFailure(r)
		writeJSON(w, http.StatusUnauthorized, json.RawMessage(`{"error":{"code":"auth_failed","message":"Unauthorized"}}`))
		return
	}
	if r.Method == http.MethodGet && path == "/api/ws" {
		s.ws(w, r)
		return
	}
	if path == STTPrefix || strings.HasPrefix(path, STTPrefix+"/") {
		if s.STT == nil {
			writeErr(w, http.StatusNotFound, "no speech-to-text server configured (even-cc config sttUrl <url>)")
			return
		}
		s.STT.ServeHTTP(w, r)
		return
	}

	// A provider parameter (the app sends provider=claude) is ignored.
	body, ok := s.parseBody(w, r)
	if !ok {
		return
	}

	switch {
	case r.Method == http.MethodGet && path == "/api/sessions":
		s.sessions(w, r)
	case r.Method == http.MethodGet && path == "/api/machine":
		s.machine(w)
	case r.Method == http.MethodGet && path == "/api/ecc/health":
		s.health(w)
	case r.Method == http.MethodPost && path == "/api/prompt":
		s.prompt(w, body)
	case r.Method == http.MethodPost && path == "/api/release":
		s.release(w, body)
	case r.Method == http.MethodPost && path == "/api/shutdown" && s.Shutdown != nil && isLoopback(r.RemoteAddr):
		writeJSON(w, 200, okBody)
		go s.Shutdown()
	case r.Method == http.MethodPost && path == "/api/permission-response":
		s.permissionResponse(w, body)
	case r.Method == http.MethodPost && path == "/api/question-response":
		s.questionResponse(w, body)
	case r.Method == http.MethodPost && path == "/api/interrupt":
		s.interrupt(w, body)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/sessions/") && strings.HasSuffix(path, "/history"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/sessions/"), "/history")
		if id == "" || strings.Contains(id, "/") {
			notFound(w, r)
			return
		}
		s.history(w, r, id)
	default:
		notFound(w, r)
	}
}

// logAuthFailure logs a rejected request, at most authLogBurst per
// authLogWindow, so a flood can't rotate the log away.
func (s *Server) logAuthFailure(r *http.Request) {
	s.authMu.Lock()
	now := time.Now()
	if now.Sub(s.authWindow) >= authLogWindow {
		if s.authSuppressed > 0 {
			s.Log.Warn("auth failures not logged in the last window", "count", s.authSuppressed)
		}
		s.authWindow, s.authLogged, s.authSuppressed = now, 0, 0
	}
	ok := s.authLogged < authLogBurst
	if ok {
		s.authLogged++
	} else {
		s.authSuppressed++
	}
	s.authMu.Unlock()
	if ok {
		s.Log.Warn("auth failed", "method", r.Method, "url", redact(r.URL), "remote", r.RemoteAddr)
	}
}

// health is GET /api/ecc/health: live sessions, their claude processes and
// WebSocket clients, recovered panics. even-cc's own; the app never calls it.
func (s *Server) health(w http.ResponseWriter) {
	writeJSON(w, 200, struct {
		Version       string                 `json:"version"`
		Pid           int                    `json:"pid"`
		UptimeSeconds int64                  `json:"uptimeSeconds"`
		Goroutines    int                    `json:"goroutines"`
		Panics        int64                  `json:"panics"`
		Clients       int                    `json:"clients"`
		Sessions      []claude.SessionHealth `json:"sessions"`
	}{s.Version, os.Getpid(), int64(time.Since(started).Seconds()), runtime.NumGoroutine(), guard.Panics.Load(), s.Hub.ClientCount(), s.Mgr.Health()})
}

func (s *Server) authorized(r *http.Request) bool {
	var provided string
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		provided = h[len("Bearer "):]
	} else {
		vals := r.URL.Query()["token"]
		if len(vals) != 1 {
			return false
		}
		provided = vals[0]
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.Token)) == 1
}

func jsString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []string:
		return strings.Join(t, ",")
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return "[object Object]"
}

func (s *Server) parseBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	body := map[string]any{}
	if r.Body == nil || !strings.Contains(r.Header.Get("Content-Type"), "json") {
		return body, true
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeErr(w, http.StatusRequestEntityTooLarge, "request body over "+strconv.Itoa(maxBody)+" bytes")
		return nil, false
	}
	if err != nil {
		s.Log.Info("request body read failed", "url", redact(r.URL), "err", err)
		return body, true
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return body, true
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		writeErr(w, 400, "Invalid JSON body: "+err.Error())
		return nil, false
	}
	if m, ok := v.(map[string]any); ok {
		return m, true
	}
	return body, true
}

func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// jsNumber is JS Number(s) (NaN -> 0 here, since callers use `|| default`).
func jsNumber(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) {
		return 0
	}
	return f
}

// jsParseInt is JS parseInt(s) (NaN -> 0).
func jsParseInt(s string) int {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && (s[end] >= '0' && s[end] <= '9' || (end == 0 && (s[end] == '-' || s[end] == '+'))) {
		end++
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}

type sessionItem struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Timestamp string  `json:"timestamp"`
	Cwd       string  `json:"cwd"`
	Provider  string  `json:"provider"`
	Status    *string `json:"status"`
	// OpenElsewhere names the client (VS Code, a terminal, ...) holding the
	// session; prompts to it are refused until it closes there (or forked).
	OpenElsewhere string `json:"openElsewhere,omitempty"`
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := int(jsNumber(q.Get("limit")))
	if limit == 0 {
		limit = 50
	}
	if limit < 0 {
		limit = 0
	}
	list, err := s.Store.List(limit, q.Get("cwd"))
	if err != nil {
		writeJSON(w, 200, struct {
			Sessions []sessionItem `json:"sessions"`
			Error    string        `json:"error"`
		}{[]sessionItem{}, err.Error()})
		return
	}
	items := make([]sessionItem, 0, len(list))
	held := s.Mgr.OpenElsewhere()
	for _, info := range list {
		it := sessionItem{ID: info.ID, Title: info.Title, Timestamp: isoTime(time.UnixMilli(info.Mtime)), Cwd: info.Cwd, Provider: "claude"}
		if e, ok := held[info.ID]; ok {
			it.OpenElsewhere = e.Where
		}
		st := s.Mgr.StatusOf(info.ID) // in memory
		it.Status = &st
		items = append(items, it)
	}
	writeJSON(w, 200, struct {
		Sessions []sessionItem `json:"sessions"`
	}{items})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request, id string) {
	// The Even app always asks for 10 (even-terminal's cap); even-cc's own
	// glasses app asks for more.
	limit := jsParseInt(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, maxHistory)
	items, err := s.Store.History(id, limit)
	if err != nil {
		writeJSON(w, 200, struct {
			History []transcript.HistoryItem `json:"history"`
			Error   string                   `json:"error"`
		}{[]transcript.HistoryItem{}, err.Error()})
		return
	}
	where, busy := "", false
	if e, ok := s.Mgr.HeldElsewhere(id); ok {
		where, busy = e.Where, s.Store.TurnOpen(id)
	}
	writeJSON(w, 200, struct {
		History       []transcript.HistoryItem `json:"history"`
		OpenElsewhere string                   `json:"openElsewhere,omitempty"`
		// ElsewhereBusy: the holder looks mid-turn (or waiting on a prompt).
		ElsewhereBusy bool `json:"elsewhereBusy,omitempty"`
	}{items, where, busy})
}

func bodyString(body map[string]any, key string) string {
	v, ok := body[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if b, ok := v.(bool); ok && !b {
		return ""
	}
	if f, ok := v.(float64); ok && f == 0 {
		return ""
	}
	return jsString(v)
}

func (s *Server) prompt(w http.ResponseWriter, body map[string]any) {
	text, ok := body["text"].(string)
	sid := bodyString(body, "sessionId")
	logText := text
	if r := []rune(logText); len(r) > 80 {
		logText = string(r[:80])
	}
	s.Log.Info("prompt", "session", sid, "text", logText)
	if !ok || text == "" {
		writeErr(w, 400, "Missing 'text' field")
		return
	}
	var cwd *string
	if c, ok := body["cwd"]; ok && c != nil {
		cs := jsString(c)
		cwd = &cs
	}
	var id string
	var err error
	if fork, _ := body["fork"].(bool); fork && sid != "" {
		id, err = s.Mgr.Fork(sid, text)
	} else {
		id, err = s.Mgr.Prompt(sid, text, cwd)
	}
	if err != nil {
		var he *claude.HTTPError
		if errors.As(err, &he) {
			writeErr(w, he.Code, he.Msg)
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 202, struct {
		OK        bool   `json:"ok"`
		SessionID string `json:"sessionId"`
		Provider  string `json:"provider"`
	}{true, id, "claude"})
}

var okBody = json.RawMessage(`{"ok":true}`)

func (s *Server) lookup(w http.ResponseWriter, body map[string]any) *claude.Session {
	sid := bodyString(body, "sessionId")
	if sid == "" {
		writeErr(w, 400, "Missing 'sessionId'")
		return nil
	}
	sess := s.Mgr.Lookup(sid)
	if sess == nil {
		writeErr(w, 404, "Session not found")
		return nil
	}
	return sess
}

func (s *Server) permissionResponse(w http.ResponseWriter, body map[string]any) {
	sess := s.lookup(w, body)
	if sess == nil {
		return
	}
	decision := bodyString(body, "decision")
	if decision == "" {
		decision = "deny"
	}
	if !sess.RespondPermission(decision) {
		writeErr(w, 400, "Permission decision was not offered or no permission request is pending")
		return
	}
	writeJSON(w, 200, okBody)
}

func (s *Server) questionResponse(w http.ResponseWriter, body map[string]any) {
	sess := s.lookup(w, body)
	if sess == nil {
		return
	}
	answer := ""
	switch v := body["answer"].(type) {
	case string:
		answer = v
	case nil:
	default:
		answer = string(jsonx.Marshal(v))
	}
	sess.RespondQuestion(answer)
	writeJSON(w, 200, okBody)
}

func (s *Server) interrupt(w http.ResponseWriter, body map[string]any) {
	sess := s.lookup(w, body)
	if sess == nil {
		return
	}
	s.Log.Info("interrupt", "session", bodyString(body, "sessionId"), "state", sess.State())
	sess.Interrupt()
	writeJSON(w, 200, okBody)
}

// machine: who this machine runs Claude as, and how much of the plan is used.
func (s *Server) machine(w http.ResponseWriter) {
	out := struct {
		Name    string        `json:"name"`
		Account any           `json:"account"`
		Usage   *claude.Usage `json:"usage"`
		Version string        `json:"version"`
	}{Name: s.Name, Account: struct{}{}, Usage: s.Mgr.Usage()}
	if s.Info != nil {
		i := s.Info.Get()
		out.Account, out.Version = i.Account, i.Version
	}
	writeJSON(w, 200, out)
}

// release closes the Claude Code process holding a session elsewhere
// (VS Code, a terminal) so prompts here continue it. {sessionId, force}:
// without force, a holder that looks mid-turn gets a 409 instead.
func (s *Server) release(w http.ResponseWriter, body map[string]any) {
	sid := bodyString(body, "sessionId")
	if sid == "" {
		writeErr(w, 400, "Missing 'sessionId'")
		return
	}
	force, _ := body["force"].(bool)
	if err := s.Mgr.Release(sid, force); err != nil {
		var he *claude.HTTPError
		if errors.As(err, &he) {
			writeErr(w, he.Code, he.Msg)
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, okBody)
}

// isLoopback: the request came from this machine.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
