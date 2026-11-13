package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestValidTarget(t *testing.T) {
	for _, ok := range []string{"Agent:1.2", "web-app:1.1", "%12", "my session:0.0"} {
		if !validTarget(ok) {
			t.Errorf("validTarget(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "a'b", "a\nrun-shell x", "a;b", "a$b", "a`b", "a\\b", `a"b`, "a\x00"} {
		if validTarget(bad) {
			t.Errorf("validTarget(%q) = true", bad)
		}
	}
}

func TestOriginGuard(t *testing.T) {
	h := originGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	cases := []struct {
		name, method, host, path string
		hdr                      map[string]string
		want                     int
	}{
		{"curl, no origin", "POST", "127.0.0.1:7685", "/api/send/x", nil, 200},
		{"dashboard itself", "POST", "127.0.0.1:7685", "/api/send/x",
			map[string]string{"Origin": "http://127.0.0.1:7685", "Sec-Fetch-Site": "same-origin"}, 200},
		{"phone over tailscale", "POST", "mac.tail1.ts.net", "/api/send/x",
			map[string]string{"Origin": "https://mac.tail1.ts.net"}, 200},
		{"tailscale serve rewrote host", "POST", "127.0.0.1:7685", "/api/send/x",
			map[string]string{"Origin": "https://mac.tail1.ts.net", "X-Forwarded-Host": "mac.tail1.ts.net"}, 200},
		{"extension", "POST", "127.0.0.1:7685", "/api/send/x",
			map[string]string{"Origin": "chrome-extension://abc"}, 200},
		{"other site POST", "POST", "127.0.0.1:7685", "/api/send/x",
			map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}, 403},
		{"other port on localhost", "POST", "127.0.0.1:7685", "/api/pane/kill",
			map[string]string{"Origin": "http://127.0.0.1:3000", "Sec-Fetch-Site": "same-site"}, 403},
		{"other site websocket", "GET", "127.0.0.1:7685", "/ws/Agent:1.1",
			map[string]string{"Origin": "https://evil.example"}, 403},
		{"other site <img> GET", "GET", "127.0.0.1:7685", "/api/goto",
			map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"link from other site to the page", "GET", "127.0.0.1:7685", "/",
			map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate"}, 200},
		{"DNS rebinding", "GET", "evil.example:7685", "/api/feed", nil, 403},
		{"localhost name", "GET", "localhost:7685", "/api/feed", nil, 200},
		{"IPv6 loopback", "GET", "[::1]:7685", "/api/feed", nil, 200},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://x"+c.path, nil)
		r.Host = c.host
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, w.Code, c.want)
		}
	}
}

func TestCleanHookEvent(t *testing.T) {
	home, _ := os.UserHomeDir()
	good := filepath.Join(home, ".claude", "projects", "x", "0a1b-2c.jsonl")
	e := hookEvent{SessionID: "0a1b2c3d-4e5f-6789-abcd-ef0123456789", TranscriptPath: good}
	if !cleanHookEvent(&e) || e.TranscriptPath != good {
		t.Fatalf("good event changed: %+v", e)
	}
	for _, p := range []string{"/etc/passwd", home + "/../x.jsonl", "rel/x.jsonl", filepath.Join(home, "notes.txt")} {
		e := hookEvent{SessionID: "abc", TranscriptPath: p}
		if !cleanHookEvent(&e) || e.TranscriptPath != "" {
			t.Errorf("path %q kept", p)
		}
	}
	for _, id := range []string{"../../etc", "a/b", "a b"} {
		e := hookEvent{SessionID: id}
		if cleanHookEvent(&e) {
			t.Errorf("session id %q accepted", id)
		}
	}
}
