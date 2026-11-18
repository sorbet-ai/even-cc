package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSTTProxy(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"text":"hello glasses"}`)
	}))
	defer upstream.Close()
	stt, err := NewSTTProxy(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Token: "tok", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), STT: stt}
	hs := httptest.NewServer(s)
	defer hs.Close()

	post := func(auth string) *http.Response {
		req, _ := http.NewRequest("POST", hs.URL+"/api/stt/v1/audio/transcriptions", strings.NewReader("WAVDATA"))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := post("wrong"); r.StatusCode != 401 {
		t.Fatalf("wrong token: %d", r.StatusCode)
	}
	r := post("tok")
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(body), "hello glasses") {
		t.Fatalf("proxied: %d %s", r.StatusCode, body)
	}
	if gotPath != "/v1/audio/transcriptions" || gotAuth != "" || gotBody != "WAVDATA" {
		t.Fatalf("upstream saw path=%q auth=%q body=%q", gotPath, gotAuth, gotBody)
	}

	off := httptest.NewServer(&Server{Token: "tok", Log: s.Log})
	defer off.Close()
	req, _ := http.NewRequest("GET", off.URL+"/api/stt/v1/models", nil)
	req.Header.Set("Authorization", "Bearer tok")
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 404 {
		t.Fatalf("unconfigured: %d", r.StatusCode)
	}
}
