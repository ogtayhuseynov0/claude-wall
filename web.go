package main

import (
	"embed"
	"io/fs"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed static
var staticFiles embed.FS

var upgrader = websocket.Upgrader{
	CheckOrigin:  func(r *http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 16384,
}

var hub *captureHub
var hooks *hookStore
var feed *feedStore

var publicMode bool

func runWeb(port int) {
	panes, err := findClaudePanes()
	if err != nil {
		fatal("detection failed: %v", err)
	}
	if len(panes) == 0 {
		fatal("no Claude Code panes found")
	}

	fmt.Printf("▸ Found %d Claude Code instance(s)\n", len(panes))

	// Register pane directories for hook matching
	for _, p := range panes {
		setPaneDir(p.Target, p.Dir)
	}

	// Start capture hub + hook store
	hub = newCaptureHub()
	hooks = newHookStore()
	feed = newFeedStore(500)
	initFinance()
	go hub.run()
	sched.load()
	go sched.run()

	// Scheduler APIs
	http.HandleFunc("/api/scheduler", handleSchedulerList)
	http.HandleFunc("/api/scheduler/create", handleSchedulerCreate)
	http.HandleFunc("/api/scheduler/", handleSchedulerAction)

	// API: list panes (re-detects each time, includes dimensions)
	http.HandleFunc("/api/panes", func(w http.ResponseWriter, r *http.Request) {
		current, err := findClaudePanes()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// Add pane dimensions
		for i := range current {
			cols := tmuxDisplayTarget(current[i].Target, "#{pane_width}", "0")
			rows := tmuxDisplayTarget(current[i].Target, "#{pane_height}", "0")
			current[i].Cols = cols
			current[i].Rows = rows
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		json.NewEncoder(w).Encode(current)
	})

	// Go to pane: switch tmux session/window/pane and activate the terminal
	http.HandleFunc("/api/goto/", func(w http.ResponseWriter, r *http.Request) {
		target := strings.TrimPrefix(r.URL.Path, "/api/goto/")
		if target == "" {
			http.Error(w, "missing target", 400)
			return
		}
		// Parse session:window.pane
		session := target
		if idx := strings.Index(target, ":"); idx >= 0 {
			session = target[:idx]
		}
		sessWin := target
		if idx := strings.Index(target, "."); idx >= 0 {
			sessWin = target[:idx]
		}

		// Switch tmux to the right session, window, and pane
		// Order matters: switch client first, then window, then pane
		// Use -t session to target the user's client (not the control mode client)
		exec.Command("tmux", "switch-client", "-t", session).Run()
		exec.Command("tmux", "select-window", "-t", sessWin).Run()
		exec.Command("tmux", "select-pane", "-t", target).Run()

		// Activate the terminal application (detect which one is running)
		activateTerminal()

		w.Write([]byte("ok"))
	})

	// Toggle zoom on tmux pane (makes it full-window so Claude re-renders wider)
	http.HandleFunc("/api/zoom/", func(w http.ResponseWriter, r *http.Request) {
		target := strings.TrimPrefix(r.URL.Path, "/api/zoom/")
		if target == "" {
			http.Error(w, "missing target", 400)
			return
		}
		exec.Command("tmux", "resize-pane", "-Z", "-t", target).Run()
		w.Write([]byte("ok"))
	})

	// Dedup recent hook events (HTTP + command hooks both fire)
	var recentHooks sync.Map // key: "sessionId:eventName:toolName" → timestamp

	// Periodically clean stale dedup entries to prevent unbounded growth
	go func() {
		for range time.NewTicker(5 * time.Minute).C {
			recentHooks.Range(func(key, value any) bool {
				if time.Since(value.(time.Time)) > 30*time.Second {
					recentHooks.Delete(key)
				}
				return true
			})
		}
	}()

	// Hook events from Claude Code instances
	http.HandleFunc("/api/hooks/event", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var event hookEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		event.ReceivedAt = time.Now()

		// Deduplicate: skip if same event received within 2 seconds
		dedupKey := event.SessionID + ":" + event.EventName + ":" + event.ToolName
		if last, ok := recentHooks.Load(dedupKey); ok {
			if time.Since(last.(time.Time)) < 2*time.Second {
				w.WriteHeader(200)
				w.Write([]byte(`{"status":"deduped"}`))
				return
			}
		}
		recentHooks.Store(dedupKey, time.Now())

		// Map TMUX_PANE to pane target — check header (HTTP hooks) then query param (command hooks)
		tmuxPane := r.Header.Get("X-Tmux-Pane")
		if tmuxPane == "" {
			tmuxPane = r.URL.Query().Get("tmux_pane")
		}
		if tmuxPane != "" {
			target := resolveTmuxPane(tmuxPane)
			if target != "" {
				event.PaneTarget = target
			}
		}

		hooks.processEvent(event)
		feed.add(buildFeedEntry(event))

		// Track costs — only on Stop events (reading full transcript is expensive)
		if (event.EventName == "Stop" || event.EventName == "StopFailure") && event.TranscriptPath != "" {
			go finance.processTranscript(event)
		}

		// Notify the hub to push a status update to matching panes
		hub.pushHookStatus(hooks)

		// Notify scheduler about pane state changes
		if event.PaneTarget != "" {
			if event.EventName == "Stop" || event.EventName == "StopFailure" {
				sched.onPaneIdle(event.PaneTarget)
			} else if event.EventName == "PreToolUse" {
				sched.onPaneWorking(event.PaneTarget)
			}
		}

		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Activity feed
	http.HandleFunc("/api/feed", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		entries := feed.getRecent(100)
		json.NewEncoder(w).Encode(entries)
	})

	// Approval queue — pending permission requests
	http.HandleFunc("/api/approvals", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		pending := feed.getPending()
		json.NewEncoder(w).Encode(pending)
	})

	// Clear a pending approval (after approve/deny)
	http.HandleFunc("/api/approvals/clear/", func(w http.ResponseWriter, r *http.Request) {
		target := strings.TrimPrefix(r.URL.Path, "/api/approvals/clear/")
		if target != "" {
			feed.clearPending(target)
		}
		w.Write([]byte("ok"))
	})

	// Send text to a pane (used by Chrome extension, CLI, etc.)
	http.HandleFunc("/api/send/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			return
		}

		target := strings.TrimPrefix(r.URL.Path, "/api/send/")
		if target == "" {
			http.Error(w, "missing target", 400)
			return
		}

		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Text == "" {
			http.Error(w, "need {\"text\":\"...\"}", 400)
			return
		}

		// Send text + Enter to the pane
		hexes := make([]string, 0, len(body.Text)+1)
		for _, b := range []byte(body.Text + "\n") {
			hexes = append(hexes, fmt.Sprintf("%02x", b))
		}
		args := append([]string{"send-keys", "-t", target, "-H"}, hexes...)
		if err := exec.Command("tmux", args...).Run(); err != nil {
			http.Error(w, "send failed", 500)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"sent"}`))
	})

	// Finance / cost tracking
	http.HandleFunc("/api/finance", handleFinanceAPI)

	// Health check
	http.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		if err := exec.Command("tmux", "info").Run(); err != nil {
			http.Error(w, "tmux not running", 503)
			return
		}
		w.Write([]byte("ok"))
	})

	// WebSocket: stream pane content + accept input
	http.HandleFunc("/ws/", handlePaneWS)

	// Mastermind: orchestrator AI chat
	http.HandleFunc("/ws/mastermind", handleMastermind)

	// UI commands: backend → frontend control
	http.HandleFunc("/api/ui/events", handleUIEvents)
	http.HandleFunc("/api/ui/", handleUIAction)

	// Serve static files (strip "static/" prefix from embedded FS)
	sub, _ := fs.Sub(staticFiles, "static")
	http.Handle("/", http.FileServer(http.FS(sub)))

	// Find port
	if port == 0 {
		port = 7685
	}
	host := "127.0.0.1"
	if publicMode {
		host = "0.0.0.0"
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		ln, err = net.Listen("tcp", fmt.Sprintf("%s:0", host))
		if err != nil {
			fatal("cannot listen: %v", err)
		}
		addr = ln.Addr().String()
	}

	fmt.Printf("▸ Dashboard at http://%s\n", addr)

	// Graceful shutdown
	var handler http.Handler = http.DefaultServeMux
	if authToken != "" && publicMode {
		fmt.Println("▸ Auth enabled (--token)")
		handler = authMiddleware(http.DefaultServeMux)
	}
	srv := &http.Server{Handler: handler}
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		fmt.Println("\n▸ Shutting down...")
		srv.Close()
	}()

	srv.Serve(ln)
}

func handlePaneWS(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimPrefix(r.URL.Path, "/ws/")
	if target == "" {
		http.Error(w, "missing pane target", 400)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Set reasonable deadlines
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	// Done channel to stop writer when reader exits
	done := make(chan struct{})

	// Track whether client has this tile focused (zoomed/active)
	var historyEnabled int32  // atomic: 0 = off, 1 = on
	var contentChanged int32  // atomic: set to 1 when live content updates arrive
	var historyRequest int32  // atomic: set to 1 for immediate one-time catchup

	// Reader: browser input → tmux send-keys
	go func() {
		defer close(done)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var input struct {
				Type string `json:"type"`
				Data string `json:"data"`
			}
			if json.Unmarshal(msg, &input) != nil {
				continue
			}
			switch input.Type {
			case "input":
				if len(input.Data) > 0 {
					hexes := make([]string, len(input.Data))
					for i, b := range []byte(input.Data) {
						hexes[i] = fmt.Sprintf("%02x", b)
					}
					args := append([]string{"send-keys", "-t", target, "-H"}, hexes...)
					exec.Command("tmux", args...).Run()
				}
			case "history-mode":
				if input.Data == "on" {
					atomic.StoreInt32(&historyEnabled, 1)
					atomic.StoreInt32(&historyRequest, 1) // immediate catchup
				} else {
					atomic.StoreInt32(&historyEnabled, 0)
				}
			}
		}
	}()

	// Send scrollback history on connect (excludes visible area to avoid overlap with live)
	lastHistory := ""
	captureHistory := func() {
		historyOut, err := exec.Command("tmux", "capture-pane", "-t", target, "-e", "-p", "-S", "-", "-E", "-1").Output()
		if err != nil || len(historyOut) <= 1 {
			return
		}
		h := string(historyOut)
		if h == lastHistory {
			return
		}
		lastHistory = h
		rawLines := strings.Split(h, "\n")
		for i, line := range rawLines {
			rawLines[i] = strings.TrimRight(line, " ")
		}
		msg, _ := json.Marshal(map[string]string{
			"type": "history",
			"data": strings.Join(rawLines, "\n"),
		})
		conn.WriteMessage(websocket.TextMessage, msg)
	}
	captureHistory()

	// Subscribe to capture hub (shared single-goroutine capture)
	updates := hub.subscribe(target)
	defer hub.unsubscribe(target, updates)

	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	// Periodically refresh scrollback history to fill the gap between
	// the initial snapshot and the live visible area
	historyTicker := time.NewTicker(5 * time.Second)
	defer historyTicker.Stop()

	for {
		select {
		case <-done:
			return

		case <-pingTicker.C:
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
				return
			}

		case <-historyTicker.C:
			// Refresh history only when: tile is focused AND (content changed OR explicit request)
			if atomic.LoadInt32(&historyEnabled) == 1 {
				if atomic.CompareAndSwapInt32(&historyRequest, 1, 0) || atomic.CompareAndSwapInt32(&contentChanged, 1, 0) {
					captureHistory()
				}
			}

		case update, ok := <-updates:
			if !ok {
				return
			}
			if update.Full {
				atomic.StoreInt32(&contentChanged, 1)
			}
			if conn.WriteMessage(websocket.TextMessage, update.Msg) != nil {
				return
			}
		}
	}
}

// authMiddleware protects all routes except hooks (which come from local Claude Code)
func authMiddleware(next http.Handler) http.Handler {
	loginPage := `<html><body style="background:#1a1c23;color:#e8eaef;font-family:sans-serif;display:flex;justify-content:center;align-items:center;height:100vh;margin:0"><form method=POST action=/auth style="display:flex;gap:8px"><input name=token type=password placeholder="Enter token" style="padding:10px 14px;font-size:14px;background:#282b36;color:#e8eaef;border:1px solid #363a4a;border-radius:6px;outline:none;width:240px"><button style="padding:10px 20px;background:#6ea1f7;color:#fff;border:none;border-radius:6px;cursor:pointer;font-size:14px">Login</button></form></body></html>`

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hooks bypass auth (local Claude Code)
		if strings.HasPrefix(r.URL.Path, "/api/hooks/") {
			next.ServeHTTP(w, r)
			return
		}

		// Login form handler
		if r.URL.Path == "/auth" && r.Method == "POST" {
			r.ParseForm()
			if r.FormValue("token") == authToken {
				http.SetCookie(w, &http.Cookie{Name: "cw-token", Value: authToken, Path: "/", MaxAge: 86400 * 30})
				http.Redirect(w, r, "/", http.StatusFound)
			} else {
				w.WriteHeader(401)
				fmt.Fprint(w, loginPage)
			}
			return
		}

		// Check auth: cookie or query param
		token := r.URL.Query().Get("token")
		if token == "" {
			if c, err := r.Cookie("cw-token"); err == nil {
				token = c.Value
			}
		}
		if token != authToken {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(401)
			fmt.Fprint(w, loginPage)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// activateTerminal detects the running terminal emulator and brings it to front.
// Supports iTerm2, Ghostty, Terminal.app. On Linux, uses wmctrl if available.
func activateTerminal() {
	if isRunning("iTerm2") {
		exec.Command("osascript", "-e", `tell application "iTerm2" to activate`).Run()
	} else if isRunning("Ghostty") {
		exec.Command("osascript", "-e", `tell application "Ghostty" to activate`).Run()
	} else if isRunning("Terminal") {
		exec.Command("osascript", "-e", `tell application "Terminal" to activate`).Run()
	} else if isRunning("Alacritty") {
		exec.Command("osascript", "-e", `tell application "Alacritty" to activate`).Run()
	} else if isRunning("WezTerm") {
		exec.Command("osascript", "-e", `tell application "WezTerm" to activate`).Run()
	}
	// On Linux or if no terminal detected, tmux switch-client already handled it
}

// isRunning checks if a macOS application process is running
func isRunning(appName string) bool {
	out, err := exec.Command("pgrep", "-x", appName).Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(out))) > 0
}

// resolveTmuxPane maps a tmux pane ID (%NNN) to a pane target (Session:W.P)
func resolveTmuxPane(paneID string) string {
	out, err := tmuxOutput("list-panes", "-a", "-F", "#{pane_id}\t#{session_name}:#{window_index}.#{pane_index}")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 && parts[0] == paneID {
			return parts[1]
		}
	}
	return ""
}

