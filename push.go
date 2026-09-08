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

// subRecord is one device subscription plus its per-event preferences.
type subRecord struct {
	Sub        webpush.Subscription `json:"sub"`
	Permission bool                 `json:"permission"`
	Stop       bool                 `json:"stop"`
	Error      bool                 `json:"error"`
	QuietStart int                  `json:"quietStart"` // hour 0-23, -1 = disabled
	QuietEnd   int                  `json:"quietEnd"`
}

func defaultRecord(s webpush.Subscription) subRecord {
	return subRecord{Sub: s, Permission: true, Stop: true, Error: true, QuietStart: -1, QuietEnd: -1}
}

// wants reports whether this device wants a push for the given event category,
// honoring its per-event toggles and quiet-hours window (server local time).
func (r subRecord) wants(cat string) bool {
	switch cat {
	case "permission":
		if !r.Permission {
			return false
		}
	case "stop":
		if !r.Stop {
			return false
		}
	case "error":
		if !r.Error {
			return false
		}
	case "test":
		return true // manual test always goes through
	}
	if r.QuietStart >= 0 && r.QuietEnd >= 0 && r.QuietStart != r.QuietEnd {
		h := time.Now().Hour()
		inQuiet := false
		if r.QuietStart < r.QuietEnd {
			inQuiet = h >= r.QuietStart && h < r.QuietEnd
		} else { // window wraps midnight, e.g. 22 → 7
			inQuiet = h >= r.QuietStart || h < r.QuietEnd
		}
		if inQuiet {
			return false
		}
	}
	return true
}

type pushStore struct {
	mu      sync.Mutex
	privKey string
	pubKey  string
	subs    []subRecord
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
	// subscriptions — new format ([]subRecord), with fallback to the old
	// flat []webpush.Subscription written by earlier versions.
	if b, err := os.ReadFile(filepath.Join(pushDir(), "push-subs.json")); err == nil {
		var recs []subRecord
		if json.Unmarshal(b, &recs) == nil && (len(recs) == 0 || recs[0].Sub.Endpoint != "") {
			p.subs = recs
		} else {
			var old []webpush.Subscription
			if json.Unmarshal(b, &old) == nil {
				for _, s := range old {
					p.subs = append(p.subs, defaultRecord(s))
				}
				p.saveSubs() // rewrite in the new format
			}
		}
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
		if e.Sub.Endpoint == s.Endpoint {
			return
		}
	}
	p.subs = append(p.subs, defaultRecord(s))
	p.saveSubs()
}

func (p *pushStore) remove(endpoint string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.subs[:0]
	for _, e := range p.subs {
		if e.Sub.Endpoint != endpoint {
			out = append(out, e)
		}
	}
	p.subs = out
	p.saveSubs()
}

func (p *pushStore) setPrefs(endpoint string, perm, stop, err bool, qs, qe int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.subs {
		if p.subs[i].Sub.Endpoint == endpoint {
			p.subs[i].Permission = perm
			p.subs[i].Stop = stop
			p.subs[i].Error = err
			p.subs[i].QuietStart = qs
			p.subs[i].QuietEnd = qe
			p.saveSubs()
			return
		}
	}
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
	Target string `json:"target,omitempty"` // pane target, for the notification's Mute action
	Attn   int    `json:"attn"`             // panes needing attention → drives the app-icon badge
}

// per-session notification mute (from the notification's "Mute 30m" action).
// Global for the single user across devices.
var muteMu sync.Mutex
var muteUntil = map[string]time.Time{}

func pushMute(target string, d time.Duration) {
	muteMu.Lock()
	muteUntil[target] = time.Now().Add(d)
	muteMu.Unlock()
}

func pushMuted(target string) bool {
	muteMu.Lock()
	defer muteMu.Unlock()
	t, ok := muteUntil[target]
	return ok && time.Now().Before(t)
}

// sendEvent delivers to every subscription that wants this category, honoring
// per-device toggles + quiet hours; prunes subscriptions the push service expired.
func (p *pushStore) sendEvent(cat string, pl pushPayload) {
	p.mu.Lock()
	priv, pub := p.privKey, p.pubKey
	recs := make([]subRecord, len(p.subs))
	copy(recs, p.subs)
	p.mu.Unlock()
	if priv == "" || len(recs) == 0 {
		return
	}
	body, _ := json.Marshal(pl)
	var dead []string
	for _, r := range recs {
		if !r.wants(cat) {
			continue
		}
		s := r.Sub
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

func handlePushPrefs(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Endpoint   string `json:"endpoint"`
		Permission bool   `json:"permission"`
		Stop       bool   `json:"stop"`
		Error      bool   `json:"error"`
		QuietStart int    `json:"quietStart"`
		QuietEnd   int    `json:"quietEnd"`
	}
	if json.NewDecoder(r.Body).Decode(&p) != nil || p.Endpoint == "" {
		http.Error(w, "bad prefs", 400)
		return
	}
	push.setPrefs(p.Endpoint, p.Permission, p.Stop, p.Error, p.QuietStart, p.QuietEnd)
	w.WriteHeader(200)
}

func handlePushTest(w http.ResponseWriter, r *http.Request) {
	push.sendEvent("test", pushPayload{
		Title: "Claude Wall", Body: "Notifications are on ✓", Tag: "cw-test", URL: "/m.html?attn=1",
	})
	w.WriteHeader(200)
}

func handlePushMute(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Target  string `json:"target"`
		Minutes int    `json:"minutes"`
	}
	if json.NewDecoder(r.Body).Decode(&p) != nil || p.Target == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if p.Minutes <= 0 {
		p.Minutes = 30
	}
	pushMute(p.Target, time.Duration(p.Minutes)*time.Minute)
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
			// attention = panes that want the user: waiting for permission or stopped/idle-stale
			attn := 0
			cur := make(map[string]string, len(s.Panes))
			for _, p := range s.Panes {
				cur[p.Target] = p.Status
				if p.Status == "permission" || p.Status == "stale" {
					attn++
				}
			}
			for _, p := range s.Panes {
				if first || pushMuted(p.Target) {
					continue
				}
				name := p.DirName
				if name == "" {
					name = p.Target
				}
				old := prev[p.Target]
				link := "/pip.html?m=1&target=" + url.QueryEscape(p.Target)
				switch {
				case p.Status == "permission" && old != "permission":
					push.sendEvent("permission", pushPayload{
						Title: "Permission needed", Body: name + " is waiting for you",
						Tag: "perm:" + p.Target, Status: "permission", URL: link, Target: p.Target, Attn: attn,
					})
				case (p.Status == "idle" || p.Status == "stale") && old == "working":
					push.sendEvent("stop", pushPayload{
						Title: "Claude stopped", Body: name + " finished / is idle",
						Tag: "stop:" + p.Target, Status: p.Status, URL: link, Target: p.Target, Attn: attn,
					})
				case p.Status == "error" && old != "error":
					push.sendEvent("error", pushPayload{
						Title: "Session error", Body: name + " hit an error",
						Tag: "err:" + p.Target, Status: "error", URL: link, Target: p.Target, Attn: attn,
					})
				}
			}
			prev = cur
			first = false
		}
	}()
}
