package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSetupPage(t *testing.T) {
	s := &Server{Token: "tok", Name: "mule (WSL)", Mirror: "g2mirror://tok@10.0.0.2:8738", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	hs := httptest.NewServer(s)
	defer hs.Close()

	if r, _ := http.Get(hs.URL + SetupPath + "?token=wrong"); r.StatusCode != 401 {
		t.Fatalf("wrong token: %d", r.StatusCode)
	}
	r, err := http.Get(hs.URL + SetupPath + "?token=tok")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	page := string(b)
	if r.StatusCode != 200 || !strings.Contains(page, "#Intent;scheme=evencc;package=com.faceclaw.app;end") {
		t.Fatalf("page %d:\n%s", r.StatusCode, page)
	}

	link, err := url.Parse(s.AddLink("10.0.0.2:3456"))
	if err != nil {
		t.Fatal(err)
	}
	q := link.Query()
	if link.Scheme != "evencc" || link.Host != "add" || q.Get("url") != "http://10.0.0.2:3456" || q.Get("token") != "tok" ||
		q.Get("name") != "mule (WSL)" || q.Get("terminal") != "g2mirror://tok@10.0.0.2:8738" || q.Has("stt") {
		t.Fatalf("link %s", link)
	}
}
