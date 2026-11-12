package main

import (
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The server can type into every terminal, so a web page on another site
// must not reach it. A page can try three ways, and the guard stops each:
//   - DNS rebinding: its own name points at 127.0.0.1, so the Host header is
//     a foreign name. Only IP literals, localhost, .ts.net, .local and this
//     machine's name are served.
//   - Cross-site requests and websockets: the browser adds a foreign Origin
//     (or Sec-Fetch-Site: cross-site / same-site for other ports). These are
//     refused on /api/ and /ws/.
//   - Pane names with quotes or newlines, which went into tmux and sh
//     command strings: validTarget refuses them.
// Clients that send no Origin (curl, hooks, the CLI, Go code) pass.

// validTarget reports whether t is safe to put inside a single-quoted tmux
// or sh argument: no quotes, backslashes, ;, $, backtick or control chars.
func validTarget(t string) bool {
	if t == "" || len(t) > 200 {
		return false
	}
	for _, c := range t {
		if c < 0x20 || c == 0x7f || strings.ContainsRune("'\"\\;$`", c) {
			return false
		}
	}
	return true
}

var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// cleanHookEvent checks the hook fields the server uses as file paths:
// session_id is joined into ~/.claude/tasks/<id>, and transcript_path is
// read by the cost, Fleet Log and webhook code. A bad session_id refuses the
// event; a transcript_path outside the home folder, or not a clean .jsonl
// path, is dropped.
func cleanHookEvent(e *hookEvent) bool {
	if e.SessionID != "" && !sessionIDRe.MatchString(e.SessionID) {
		return false
	}
	if p := e.TranscriptPath; p != "" {
		home, _ := os.UserHomeDir()
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || !strings.HasSuffix(p, ".jsonl") ||
			home == "" || !strings.HasPrefix(p, home+string(filepath.Separator)) {
			e.TranscriptPath = ""
		}
	}
	return true
}

func allowedHost(host string) bool {
	if host == "" {
		return true // HTTP/1.0 or a non-browser client
	}
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.ToLower(strings.TrimSuffix(strings.Trim(h, "[]"), "."))
	if net.ParseIP(h) != nil || h == "localhost" || strings.HasSuffix(h, ".localhost") ||
		strings.HasSuffix(h, ".ts.net") || strings.HasSuffix(h, ".local") {
		return true
	}
	if name, err := os.Hostname(); err == nil {
		name = strings.ToLower(strings.TrimSuffix(name, ".local"))
		if h == name || h == name+".local" {
			return true
		}
	}
	return false
}

// originAllowed reports whether a browser request comes from the dashboard
// itself (same host), from the Chrome extension, or from no page at all.
func originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		switch r.Header.Get("Sec-Fetch-Site") {
		case "cross-site", "same-site":
			return false // e.g. an <img> or <form> on another site
		}
		return true
	}
	if strings.HasPrefix(o, "chrome-extension://") {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Host)
	if host == strings.ToLower(r.Host) {
		return true
	}
	// tailscale serve may pass the original host here.
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" && host == strings.ToLower(fh) {
		return true
	}
	return false
}

func originGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedHost(r.Host) {
			log.Printf("[guard] blocked host %q: %s %s", r.Host, r.Method, r.URL.Path)
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		p := r.URL.Path
		if (strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/ws/") || r.Method != "GET" && r.Method != "HEAD") &&
			!originAllowed(r) {
			log.Printf("[guard] blocked origin %q (fetch-site %q): %s %s",
				r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Site"), r.Method, p)
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
