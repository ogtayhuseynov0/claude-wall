package main

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Reverse-proxy local ports to the phone through the single Tailscale tunnel
// (:7685): /proxy/<port>/<path> → http://127.0.0.1:<port>/<path>. The port is
// the only input and the host is fixed to 127.0.0.1, so this can reach only
// services already listening on this machine (no SSRF to arbitrary hosts).

var proxyPathRe = regexp.MustCompile(`^/proxy/(\d{1,5})(/.*)?$`)

func handleProxy(w http.ResponseWriter, r *http.Request) {
	m := proxyPathRe.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.Error(w, "usage: /proxy/<port>/", http.StatusBadRequest)
		return
	}
	port := m[1]
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		http.Error(w, "bad port", http.StatusBadRequest)
		return
	}
	rest := m[2]
	if rest == "" { // need a trailing slash so the injected <base> resolves
		http.Redirect(w, r, "/proxy/"+port+"/", http.StatusFound)
		return
	}
	proxyTo(w, r, port, rest)
}

// proxyTo forwards one request to 127.0.0.1:<port> at <path>. Used both by the
// /proxy/<port>/ route and by the root asset fallback (SPA absolute paths).
func proxyTo(w http.ResponseWriter, r *http.Request, port, path string) {
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + port}
	prefix := "/proxy/" + port
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = path
			req.Host = target.Host
			req.Header.Set("Accept-Encoding", "identity") // plain body so we can inject <base>
		},
		ModifyResponse: func(resp *http.Response) error {
			// Keep server-driven redirects inside the proxy.
			if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") {
				resp.Header.Set("Location", prefix+loc)
			}
			// Inject <base> into HTML so relative asset paths resolve under the
			// prefix; root-absolute paths are caught by the cookie fallback.
			if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					return err
				}
				tag := []byte(`<base href="` + prefix + `/">`)
				if i := bytes.Index(bytes.ToLower(body), []byte("<head>")); i >= 0 {
					body = append(body[:i+6], append(tag, body[i+6:]...)...)
				} else {
					body = append(tag, body...)
				}
				resp.Body = io.NopCloser(bytes.NewReader(body))
				resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
				resp.Header.Del("Content-Encoding")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, e error) {
			http.Error(w, "port "+port+" not reachable: "+e.Error(), http.StatusBadGateway)
		},
	}
	// Remember the port so root-absolute asset requests (/assets/…, /_next/…)
	// route to the same backend.
	http.SetCookie(w, &http.Cookie{Name: "cwproxy", Value: port, Path: "/"})
	rp.ServeHTTP(w, r)
}

// staticExists reports whether p is one of claude-wall's own embedded files, so
// the root handler serves those itself and only falls back to the proxy for
// paths the backend owns.
func staticExists(sub fs.FS, p string) bool {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return true
	}
	f, err := sub.Open(p)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

var lsofAddrRe = regexp.MustCompile(`(\S+):(\d+) \(LISTEN\)`)

type portInfo struct {
	Port int    `json:"port"`
	Proc string `json:"proc"`
	All  bool   `json:"all"` // listens on all interfaces (reachable directly over Tailscale too)
}

// handlePorts lists local TCP listeners so the Ports page can offer them.
func handlePorts(w http.ResponseWriter, r *http.Request) {
	out, _ := exec.Command("lsof", "-nP", "-iTCP", "-sTCP:LISTEN").Output()
	byPort := map[int]portInfo{}
	for _, line := range strings.Split(string(out), "\n") {
		am := lsofAddrRe.FindStringSubmatch(line)
		if am == nil {
			continue
		}
		port, _ := strconv.Atoi(am[2])
		if port <= 0 {
			continue
		}
		host := am[1]
		all := host == "*" || host == "0.0.0.0" || host == "[::]"
		proc := ""
		if f := strings.Fields(line); len(f) > 0 {
			proc = strings.ReplaceAll(f[0], `\x20`, " ")
		}
		if ex, ok := byPort[port]; !ok || (!ex.All && all) {
			byPort[port] = portInfo{Port: port, Proc: proc, All: all}
		}
	}
	list := make([]portInfo, 0, len(byPort))
	for _, p := range byPort {
		if p.Port == 7685 { // that's us
			continue
		}
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Port < list[j].Port })
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(list)
}
