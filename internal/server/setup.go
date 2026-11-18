package server

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
)

// SetupPath is the page even-cc's QR code opens on the phone: it hands
// Faceclaw an evencc://add link (our Faceclaw build registers the scheme)
// carrying this server's address, token and name, so pairing is one scan
// with the camera and Faceclaw doesn't need to be open first.
const SetupPath = "/setup"

// SetupURL is what the QR code encodes. It carries everything Faceclaw needs,
// so Faceclaw's own scanner can add the PC without loading the page (phone
// browsers may refuse to reach home-network addresses).
func SetupURL(addr string, port int, token, name string, stt bool, terminal string) string {
	return fmt.Sprintf("http://%s:%d%s?%s", addr, port, SetupPath, setupQuery(token, name, stt, terminal).Encode())
}

func setupQuery(token, name string, stt bool, terminal string) url.Values {
	q := url.Values{}
	q.Set("token", token)
	q.Set("name", name)
	if stt {
		q.Set("stt", "1")
	}
	if terminal != "" {
		q.Set("terminal", terminal)
	}
	return q
}

// MirrorConn is the g2mirror:// connection string for a mirror on port.
func MirrorConn(token, addr string, port int) string {
	conn := "g2mirror://" + token + "@" + addr
	if port != 8737 {
		conn += fmt.Sprintf(":%d", port)
	}
	return conn
}

// AddLink is the evencc://add link for this server, reached at host.
func (s *Server) AddLink(host string) string {
	q := setupQuery(s.Token, s.Name, s.STT != nil, s.Mirror)
	q.Set("url", "http://"+host)
	return "evencc://add?" + q.Encode()
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.logAuthFailure(r)
		http.Error(w, "This setup link is out of date: scan the QR code even-cc prints now.", http.StatusUnauthorized)
		return
	}
	link := s.AddLink(r.Host)
	// Chrome opens intent: links in the named app; the plain link is the fallback.
	u, _ := url.Parse(link)
	intent := "intent://" + u.Host + "?" + u.RawQuery + "#Intent;scheme=evencc;package=com.faceclaw.app;end"
	s.Log.Info("setup page opened", "remote", r.RemoteAddr)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>even-cc setup</title>
<style>body{font:18px system-ui,sans-serif;margin:24px;background:#111;color:#eee}a.b{display:block;margin:20px 0;padding:16px;border-radius:10px;background:#3a7;color:#fff;text-align:center;text-decoration:none;font-weight:600}small{color:#999}</style>
</head><body>
<h2>Add %s to Faceclaw</h2>
<p>Opening Faceclaw&hellip;</p>
<a class="b" href="%s">Open in Faceclaw</a>
<p><small>Needs our Faceclaw build (with the Claude Code app). If nothing happens, tap the button.</small></p>
<script>location.href=%q;</script>
</body></html>`, html.EscapeString(s.Name), html.EscapeString(intent), intent)
}
