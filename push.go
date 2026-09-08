package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Web Push (PWA notifications). VAPID keys + subscriptions persist under
// ~/.claude/claude-wall/. A background watcher polls /api/summary and pushes
// on status edges: permission needed, or an agent that stopped working.
//
// The VAPID private key is a secret — its file is chmod 0600 and gitignored.

const vapidSubscriber = "mailto:claude-wall@localhost" // contact sent to push services; no real email leaked

type pushStore struct {
	mu      sync.Mutex
	privKey string
	pubKey  string
	subs    []webpush.Subscription
}

var push = &pushStore{}

func pushDir() string {
	home, _ := os.UserHomeDir()
	d := filepath.Join(home, ".claude", "claude-wall")
	os.MkdirAll(d, 0700)
	return d
}

func (p *pushStore) load() {
	p.mu.Lock()
	defer p.mu.Unlock()
	// VAPID keys — generate + persist on first run
	vf := filepath.Join(pushDir(), "vapid.json")
	if b, err := os.ReadFile(vf); err == nil {
		var v struct{ Priv, Pub string }
		if json.Unmarshal(b, &v) == nil && v.Priv != "" && v.Pub != "" {
			p.privKey, p.pubKey = v.Priv, v.Pub
		}
	}
	if p.privKey == "" {
		if priv, pub, err := webpush.GenerateVAPIDKeys(); err == nil {
			p.privKey, p.pubKey = priv, pub
			b, _ := json.Marshal(struct{ Priv, Pub string }{priv, pub})
			os.WriteFile(vf, b, 0600)
		}
	}
	// subscriptions
	if b, err := os.ReadFile(filepath.Join(pushDir(), "push-subs.json")); err == nil {
		json.Unmarshal(b, &p.subs)
	}
}

func (p *pushStore) saveSubs() {
	b, _ := json.MarshalIndent(p.subs, "", "  ")
	os.WriteFile(filepath.Join(pushDir(), "push-subs.json"), b, 0600)
}

func (p *pushStore) add(s webpush.Subscription) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.subs {
		if e.Endpoint == s.Endpoint {
			return
		}
	}
	p.subs = append(p.subs, s)
	p.saveSubs()
}

func (p *pushStore) remove(endpoint string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.subs[:0]
	for _, e := range p.subs {
		if e.Endpoint != endpoint {
			out = append(out, e)
		}
	}
	p.subs = out
	p.saveSubs()
}

func (p *pushStore) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.subs)
}

type pushPayload struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Tag    string `json:"tag"`
	URL    string `json:"url"`
	Status string `json:"status,omitempty"`
}

// send delivers to every subscription; prunes ones the push service has expired.
func (p *pushStore) send(pl pushPayload) {
	p.mu.Lock()
	priv, pub := p.privKey, p.pubKey
	subs := make([]webpush.Subscription, len(p.subs))
	copy(subs, p.subs)
	p.mu.Unlock()
	if priv == "" || len(subs) == 0 {
		return
	}
	body, _ := json.Marshal(pl)
	var dead []string
	for i := range subs {
		s := subs[i]
		resp, err := webpush.SendNotification(body, &s, &webpush.Options{
			Subscriber:      vapidSubscriber,
			VAPIDPublicKey:  pub,
			VAPIDPrivateKey: priv,
			TTL:             120,
			Urgency:         webpush.UrgencyHigh,
		})
		if err != nil {
			continue
		}
		if resp.StatusCode == 404 || resp.StatusCode == 410 {
			dead = append(dead, s.Endpoint)
		}
		resp.Body.Close()
	}
	for _, e := range dead {
		p.remove(e)
	}
}

func handlePushVAPID(w http.ResponseWriter, r *http.Request) {
	push.mu.Lock()
	pub := push.pubKey
	push.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"publicKey": pub})
}

func handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	var s webpush.Subscription
	if json.NewDecoder(r.Body).Decode(&s) != nil || s.Endpoint == "" {
		http.Error(w, "bad subscription", 400)
		return
	}
	push.add(s)
	w.WriteHeader(200)
}

func handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var s struct {
		Endpoint string `json:"endpoint"`
	}
	json.NewDecoder(r.Body).Decode(&s)
	if s.Endpoint != "" {
		push.remove(s.Endpoint)
	}
	w.WriteHeader(200)
}

func handlePushTest(w http.ResponseWriter, r *http.Request) {
	push.send(pushPayload{
		Title: "Claude Wall", Body: "Notifications are on ✓", Tag: "cw-test", URL: "/m.html",
	})
	w.WriteHeader(200)
}

// startNotifyWatcher polls the local summary and pushes on status transitions.
func startNotifyWatcher(port int) {
	go func() {
		client := &http.Client{Timeout: 6 * time.Second}
		summaryURL := fmt.Sprintf("http://127.0.0.1:%d/api/summary", port)
		type paneStatus struct {
			Target  string `json:"target"`
			DirName string `json:"dirName"`
			Status  string `json:"status"`
		}
		type summary struct {
			Panes []paneStatus `json:"panes"`
		}
		prev := map[string]string{}
		first := true
		for {
			time.Sleep(8 * time.Second)
			if push.count() == 0 { // nobody listening — skip the capture cost
				first = true
				continue
			}
			resp, err := client.Get(summaryURL)
			if err != nil {
				continue
			}
			var s summary
			derr := json.NewDecoder(resp.Body).Decode(&s)
			resp.Body.Close()
			if derr != nil {
				continue
			}
			cur := make(map[string]string, len(s.Panes))
			for _, p := range s.Panes {
				cur[p.Target] = p.Status
				if first {
					continue
				}
				name := p.DirName
				if name == "" {
					name = p.Target
				}
				old := prev[p.Target]
				switch {
				case p.Status == "permission" && old != "permission":
					push.send(pushPayload{
						Title: "Permission needed", Body: name + " is waiting for you",
						Tag: "perm:" + p.Target, Status: "permission",
						URL: "/pip.html?m=1&target=" + url.QueryEscape(p.Target),
					})
				case (p.Status == "idle" || p.Status == "stale") && old == "working":
					push.send(pushPayload{
						Title: "Claude stopped", Body: name + " finished / is idle",
						Tag: "stop:" + p.Target, Status: p.Status,
						URL: "/pip.html?m=1&target=" + url.QueryEscape(p.Target),
					})
				case p.Status == "error" && old != "error":
					push.send(pushPayload{
						Title: "Session error", Body: name + " hit an error",
						Tag: "err:" + p.Target, Status: "error",
						URL: "/pip.html?m=1&target=" + url.QueryEscape(p.Target),
					})
				}
			}
			prev = cur
			first = false
		}
	}()
}
