package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

func chromeUserDataDir() string {
	return filepath.Join(os.Getenv("HOME"), ".claude", "claude-wall", "chrome-remote")
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

// ensureChrome launches the dedicated debug Chrome if it isn't already up.
func ensureChrome() error {
	if chromeDebugReady() {
		return nil
	}
	bin := chromePath()
	if bin == "" {
		return fmt.Errorf("Google Chrome not found in /Applications")
	}
	dir := chromeUserDataDir()
	os.MkdirAll(dir, 0700)
	cmd := exec.Command(bin,
		"--remote-debugging-port="+strconv.Itoa(chromeDebugPort),
		"--user-data-dir="+dir,
		"--no-first-run", "--no-default-browser-check",
		"--remote-allow-origins=*",
		"about:blank",
	)
	if err := cmd.Start(); err != nil {
		return err
	}
	for i := 0; i < 50; i++ {
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

// handleBrowserStart launches Chrome and reports readiness (used by the page
// before opening the socket).
func handleBrowserStart(w http.ResponseWriter, r *http.Request) {
	err := ensureChrome()
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
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
	if err := ensureChrome(); err != nil {
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
