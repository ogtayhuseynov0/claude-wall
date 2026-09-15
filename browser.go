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
// runs with a debug port (no sudo, its own profile). The phone gets a live
// screencast and forwards taps/scroll/typing back — so YOU do a login by hand
// from the phone. Chrome renders its own surface, so this works even when the
// Mac's display is asleep or the screen is locked. Keystrokes flow
// phone → this server → Chrome; the model never sees them.

const chromeDebugPort = 9222

// errNeedsRelaunch: Chrome is running but without a debug port, so we can't
// attach to your real (logged-in) profiles without restarting it.
var errNeedsRelaunch = fmt.Errorf("chrome-needs-relaunch")

// realChromeDir is your normal Chrome profile directory (all your logins/profiles).
func realChromeDir() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "Google", "Chrome")
}

const chromeMainProc = "Google Chrome.app/Contents/MacOS/Google Chrome"

func chromeRunning() bool {
	out, _ := exec.Command("pgrep", "-f", chromeMainProc).Output()
	return len(strings.TrimSpace(string(out))) > 0
}

// quitChrome asks Chrome to quit (so it saves the session for --restore-last-session),
// then force-kills if it lingers.
func quitChrome() {
	exec.Command("osascript", "-e", `tell application "Google Chrome" to quit`).Run()
	for i := 0; i < 40; i++ {
		if !chromeRunning() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	exec.Command("pkill", "-f", chromeMainProc).Run()
	time.Sleep(500 * time.Millisecond)
}

// launchChromeDebug starts your real Chrome (default profile dir) with the debug
// port, restoring the previous session. profile selects a specific profile dir
// (e.g. "Default", "Profile 2") or "" for the last used.
func launchChromeDebug(profile string) error {
	bin := chromePath()
	if bin == "" {
		return fmt.Errorf("Google Chrome not found in /Applications")
	}
	args := []string{
		"--remote-debugging-port=" + strconv.Itoa(chromeDebugPort),
		"--remote-allow-origins=*",
		"--restore-last-session",
	}
	if profile != "" {
		args = append(args, "--profile-directory="+profile)
	}
	return exec.Command(bin, args...).Start() // no --user-data-dir → your real profiles
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

// ensureChrome makes the debug endpoint available on your real Chrome. If
// Chrome is already running WITHOUT the debug port, it returns errNeedsRelaunch
// unless allowRelaunch is set (then it quits+relaunches, restoring the session).
func ensureChrome(allowRelaunch bool, profile string) error {
	if chromeDebugReady() {
		return nil
	}
	if chromeRunning() {
		if !allowRelaunch {
			return errNeedsRelaunch
		}
		quitChrome()
	}
	if err := launchChromeDebug(profile); err != nil {
		return err
	}
	for i := 0; i < 60; i++ {
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

// handleBrowserStart reports whether the debug endpoint is ready, or that Chrome
// must be relaunched to attach to your real profiles. It does NOT restart Chrome
// on its own — that's the explicit /api/browser/relaunch step.
func handleBrowserStart(w http.ResponseWriter, r *http.Request) {
	err := ensureChrome(false, "")
	w.Header().Set("Content-Type", "application/json")
	switch {
	case err == nil:
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	case err == errNeedsRelaunch:
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "needsRelaunch": true})
	default:
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
	}
}

// handleBrowserRelaunch quits your Chrome and relaunches it with the debug port
// (session restored). Destructive-ish (closes current windows briefly), so the
// UI confirms first. Optional ?profile=<dir> opens a specific profile.
func handleBrowserRelaunch(w http.ResponseWriter, r *http.Request) {
	err := ensureChrome(true, r.URL.Query().Get("profile"))
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
	if err := ensureChrome(false, ""); err != nil {
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
