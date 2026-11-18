package server

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// STTPrefix is where even-cc forwards speech-to-text requests to a local
// OpenAI-compatible Whisper server (e.g. Speaches in Docker, which on Windows
// is reachable only from this PC). The phone points its Whisper server
// setting at http://<pc>:<port>/api/stt/v1 with the pairing token as the key.
const STTPrefix = "/api/stt"

// NewSTTProxy forwards /api/stt/<path> to <target>/<path>, dropping the
// caller's Authorization (it carries even-cc's token, not the server's).
func NewSTTProxy(target string) (http.Handler, error) {
	u, err := url.Parse(strings.TrimRight(target, "/"))
	if err != nil {
		return nil, err
	}
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(u)
			r.Out.URL.Path = u.Path + strings.TrimPrefix(r.In.URL.Path, STTPrefix)
			r.Out.URL.RawPath = ""
			r.Out.Header.Del("Authorization")
			r.Out.Host = u.Host
		},
	}, nil
}
