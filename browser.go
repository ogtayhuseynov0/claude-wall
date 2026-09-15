package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Remote browser control via Chrome DevTools Protocol (CDP). A dedicated Chrome
// runs headless (--headless=new) with a debug port on a cloned profile (no
// sudo); headless=new still loads your extensions, so your logins AND
// extensions are available, and its offscreen renderer keeps producing frames
// even when the Mac's display is asleep or the screen is locked. The phone gets
// a live screencast and forwards taps/scroll/typing back — so YOU do a login by
// hand. Keystrokes flow phone → this server → Chrome; the model never sees them.

const chromeDebugPort = 9222

// realChromeDir is your normal Chrome profile directory (all your logins/profiles).
func realChromeDir() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "Google", "Chrome")
}

// attachDir is a dedicated user-data-dir cloned from your real profile. Chrome
// 136+ IGNORES --remote-debugging-port on the default dir, so we debug a clone
// instead — same-Mac Keychain decrypts the copied cookies, so your logins work.
func attachDir() string {
	return filepath.Join(os.Getenv("HOME"), ".claude", "claude-wall", "chrome-attach")
}

// syncProfile clones your real Chrome profile into attachDir, minus caches and
// lock files. Chrome must be quit first for a consistent copy.
func syncProfile() error {
	src := realChromeDir()
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("chrome profile not found at %s", src)
	}
	dst := attachDir()
	// Absolute safety: never let the clone target be, contain, or sit inside the
	// real profile — otherwise --delete could wipe real data.
	if dst == src || strings.HasPrefix(dst+"/", src+"/") || strings.HasPrefix(src+"/", dst+"/") {
		return fmt.Errorf("refusing unsafe clone paths (%s ↔ %s)", src, dst)
	}
	os.MkdirAll(dst, 0700)
	// --no-links: don't copy symlinks (a preserved symlink could point back into
	// the real profile and let the clone's Chrome write through it).
	args := []string{"-a", "--no-links", "--delete",
		"--exclude=*Cache*", "--exclude=Singleton*", "--exclude=Crashpad",
		"--exclude=*/Service Worker/CacheStorage", "--exclude=component_crx_cache",
		src + "/", dst + "/"}
	return exec.Command("rsync", args...).Run()
}

// launchChromeDebug starts a headless (--headless=new) Chrome on the clone dir
// with the debug port. Chrome 153's headless=new still loads the profile's
// installed extensions (verified: the service workers run), so your logins AND
// extensions work — while the offscreen renderer emits screencast frames no
// matter what the Mac's physical display is doing (asleep, locked, lid shut),
// which a headed window can't. Separate process on a separate user-data-dir, so
// your real Chrome is never touched.
func launchChromeDebug(profile string) error {
	bin := chromePath()
	if bin == "" {
		return fmt.Errorf("Google Chrome not found in /Applications")
	}
	args := []string{
		"--headless=new",
		"--remote-debugging-port=" + strconv.Itoa(chromeDebugPort),
		"--remote-allow-origins=*",
		"--user-data-dir=" + attachDir(),
		"--no-first-run", "--no-default-browser-check",
		// Do NOT let the clone's activity sync up to your Google account (which
		// would flow back into your real profile). Keep it a local snapshot.
		"--disable-sync",
	}
	if profile != "" {
		args = append(args, "--profile-directory="+profile)
	}
	return exec.Command(bin, args...).Start()
}

// killChromeDebug stops the headless clone by its debug-port PID (safe: only the
// clone listens on 9222 — your real Chrome doesn't). Used to switch profiles.
func killChromeDebug() {
	out, _ := exec.Command("lsof", "-ti", "tcp:"+strconv.Itoa(chromeDebugPort)).Output()
	if pids := strings.Fields(string(out)); len(pids) > 0 {
		exec.Command("kill", pids...).Run()
		time.Sleep(500 * time.Millisecond)
	}
}

func chromePath() string {
	for _, c := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Google Chrome Beta.app/Contents/MacOS/Google Chrome Beta",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func chromeDebugReady() bool {
	c := http.Client{Timeout: 800 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", chromeDebugPort))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// ensureChrome makes the debug endpoint available. Because the debug Chrome is
// a SEPARATE headless process on a cloned user-data-dir, we never quit or touch
// your real Chrome — we clone the profile (read-only source) and launch the
// headless instance. If it's already up we just reuse it (no re-sync while it's
// running, so we never rsync --delete over a live clone dir).
func ensureChrome(profile string) error {
	if chromeDebugReady() {
		return nil
	}
	if err := syncProfile(); err != nil { // clone your profile (logins) into the debug dir
		return err
	}
	if err := launchChromeDebug(profile); err != nil {
		return err
	}
	for i := 0; i < 80; i++ {
		if chromeDebugReady() {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("chrome did not become ready on port %d", chromeDebugPort)
}

type cdpTarget struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	URL   string `json:"url"`
	WSURL string `json:"webSocketDebuggerUrl"`
}

// chromePageWS returns the debugger WebSocket URL of a page target, creating one
// if none exist.
func chromePageWS() (string, error) {
	list := func() ([]cdpTarget, error) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json", chromeDebugPort))
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var ts []cdpTarget
		return ts, json.NewDecoder(resp.Body).Decode(&ts)
	}
	ts, err := list()
	if err != nil {
		return "", err
	}
	for _, t := range ts {
		if t.Type == "page" && t.WSURL != "" {
			return t.WSURL, nil
		}
	}
	// none — create one (Chrome 111+ requires PUT on /json/new)
	req, _ := http.NewRequest("PUT", fmt.Sprintf("http://127.0.0.1:%d/json/new?about:blank", chromeDebugPort), nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		var t cdpTarget
		json.NewDecoder(resp.Body).Decode(&t)
		resp.Body.Close()
		if t.WSURL != "" {
			return t.WSURL, nil
		}
	}
	return "", fmt.Errorf("no page target available")
}

// handleBrowserStart clones your profile (if needed) and launches the headless
// debug Chrome, then reports ready. Optional ?profile=<dir> picks a profile.
// Your real Chrome is never touched, so there's no relaunch prompt.
func handleBrowserStart(w http.ResponseWriter, r *http.Request) {
	err := ensureChrome(r.URL.Query().Get("profile"))
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleBrowserRelaunch restarts the headless clone, optionally on a different
// profile (?profile=<dir>) or with a fresh cookie snapshot. It kills only the
// clone (by debug-port PID) and re-clones — your real Chrome is untouched.
func handleBrowserRelaunch(w http.ResponseWriter, r *http.Request) {
	killChromeDebug()
	err := ensureChrome(r.URL.Query().Get("profile"))
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleBrowserProfiles lists your Chrome profiles (dir + display name).
func handleBrowserProfiles(w http.ResponseWriter, r *http.Request) {
	type prof struct {
		Dir  string `json:"dir"`
		Name string `json:"name"`
	}
	out := []prof{}
	if data, err := os.ReadFile(filepath.Join(realChromeDir(), "Local State")); err == nil {
		var ls struct {
			Profile struct {
				InfoCache map[string]struct {
					Name string `json:"name"`
				} `json:"info_cache"`
			} `json:"profile"`
		}
		if json.Unmarshal(data, &ls) == nil {
			for dir, info := range ls.Profile.InfoCache {
				out = append(out, prof{dir, info.Name})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// keyEvent maps a named special key to CDP key-event fields.
var keyEvents = map[string]struct {
	Key, Code string
	VK        int
}{
	"Enter":      {"Enter", "Enter", 13},
	"Backspace":  {"Backspace", "Backspace", 8},
	"Tab":        {"Tab", "Tab", 9},
	"Escape":     {"Escape", "Escape", 27},
	"ArrowUp":    {"ArrowUp", "ArrowUp", 38},
	"ArrowDown":  {"ArrowDown", "ArrowDown", 40},
	"ArrowLeft":  {"ArrowLeft", "ArrowLeft", 37},
	"ArrowRight": {"ArrowRight", "ArrowRight", 39},
}

// handleBrowserWS bridges the phone and Chrome CDP: screencast frames out,
// input in.
func handleBrowserWS(w http.ResponseWriter, r *http.Request) {
	if err := ensureChrome(""); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	pageWS, err := chromePageWS()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	phone, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer phone.Close()

	chrome, _, err := websocket.DefaultDialer.Dial(pageWS, nil)
	if err != nil {
		phone.WriteJSON(map[string]any{"type": "error", "msg": "connect chrome: " + err.Error()})
		return
	}
	defer chrome.Close()

	var mu sync.Mutex
	id := 0
	send := func(method string, params map[string]any) {
		mu.Lock()
		id++
		m := map[string]any{"id": id, "method": method}
		if params != nil {
			m["params"] = params
		}
		b, _ := json.Marshal(m)
		chrome.WriteMessage(websocket.TextMessage, b)
		mu.Unlock()
	}

	send("Page.enable", nil)
	// Render mobile-sized so pages fit the phone and login forms look right.
	send("Emulation.setDeviceMetricsOverride", map[string]any{
		"width": 420, "height": 900, "deviceScaleFactor": 2, "mobile": true,
	})
	send("Page.startScreencast", map[string]any{
		"format": "jpeg", "quality": 55, "maxWidth": 900, "maxHeight": 1900, "everyNthFrame": 1,
	})

	// Chrome → phone
	go func() {
		defer phone.Close()
		for {
			_, data, err := chrome.ReadMessage()
			if err != nil {
				return
			}
			var ev struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			switch ev.Method {
			case "Page.screencastFrame":
				var p struct {
					Data      string `json:"data"`
					SessionID int    `json:"sessionId"`
					Metadata  struct {
						DeviceWidth  float64 `json:"deviceWidth"`
						DeviceHeight float64 `json:"deviceHeight"`
					} `json:"metadata"`
				}
				json.Unmarshal(ev.Params, &p)
				send("Page.screencastFrameAck", map[string]any{"sessionId": p.SessionID})
				phone.WriteJSON(map[string]any{"type": "frame", "data": p.Data, "w": p.Metadata.DeviceWidth, "h": p.Metadata.DeviceHeight})
			case "Page.frameNavigated":
				// report URL so the address bar can update
				var p struct {
					Frame struct {
						URL    string `json:"url"`
						Parent string `json:"parentId"`
					} `json:"frame"`
				}
				if json.Unmarshal(ev.Params, &p) == nil && p.Frame.Parent == "" && p.Frame.URL != "" {
					phone.WriteJSON(map[string]any{"type": "url", "url": p.Frame.URL})
				}
			}
		}
	}()

	// phone → Chrome
	for {
		_, data, err := phone.ReadMessage()
		if err != nil {
			return
		}
		var m struct {
			Type string  `json:"type"`
			X    float64 `json:"x"`
			Y    float64 `json:"y"`
			Dy   float64 `json:"dy"`
			Text string  `json:"text"`
			Key  string  `json:"key"`
			URL  string  `json:"url"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "click":
			send("Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": m.X, "y": m.Y, "button": "left", "clickCount": 1})
			send("Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": m.X, "y": m.Y, "button": "left", "clickCount": 1})
		case "move":
			send("Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": m.X, "y": m.Y})
		case "scroll":
			send("Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": m.X, "y": m.Y, "deltaX": 0, "deltaY": m.Dy})
		case "text":
			if m.Text != "" {
				send("Input.insertText", map[string]any{"text": m.Text})
			}
		case "key":
			if k, ok := keyEvents[m.Key]; ok {
				send("Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": k.Key, "code": k.Code, "windowsVirtualKeyCode": k.VK, "nativeVirtualKeyCode": k.VK})
				send("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": k.Key, "code": k.Code, "windowsVirtualKeyCode": k.VK, "nativeVirtualKeyCode": k.VK})
			}
		case "nav":
			if m.URL != "" {
				send("Page.navigate", map[string]any{"url": m.URL})
			}
		case "back":
			send("Runtime.evaluate", map[string]any{"expression": "history.back()"})
		case "reload":
			send("Page.reload", nil)
		}
	}
}
