package main

import (
	"encoding/json"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// captureHub runs a single goroutine that captures all registered panes
// and broadcasts updates to subscribers. Replaces per-WebSocket polling.
type captureHub struct {
	mu               sync.RWMutex
	subscribers      map[string][]chan paneUpdate // target → channels
	latest           map[string]paneUpdate        // target → last update
	termWorkingUntil map[string]time.Time         // target → debounce: stay "working" until this time

	// The tmux control-mode connection, when one is up. It is held here rather
	// than as a local in run() for two reasons: shutdown has to be able to
	// detach it, and the capture loop has to be able to see it disappear and
	// come back. nil means we are on the batch-capture fallback.
	ctlMu   sync.RWMutex
	ctl     *tmuxControl
	closing bool
}

type paneUpdate struct {
	Status      string
	Activity    string // e.g. "$ npm test", "Edit main.go"
	Full        bool   // true = full content update, false = status-only
	Msg         []byte // pre-serialized JSON (built once, sent to all subscribers)
	Msg_content string // raw content for diffing
}

// paneDir maps pane target → directory (for hook matching)
var paneDirs = struct {
	sync.RWMutex
	m map[string]string
}{m: make(map[string]string)}

func setPaneDir(target, dir string) {
	paneDirs.Lock()
	paneDirs.m[target] = dir
	paneDirs.Unlock()
}

func getPaneDir(target string) string {
	paneDirs.RLock()
	defer paneDirs.RUnlock()
	return paneDirs.m[target]
}

func newCaptureHub() *captureHub {
	return &captureHub{
		subscribers:      make(map[string][]chan paneUpdate),
		latest:           make(map[string]paneUpdate),
		termWorkingUntil: make(map[string]time.Time),
	}
}

// statusSnapshot returns the last known live status per subscribed target
// (same resolution the dashboard sees). Empty for panes with no capture yet.
func (h *captureHub) statusSnapshot() map[string]string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	m := make(map[string]string, len(h.latest))
	for t, u := range h.latest {
		if u.Status != "" && u.Status != "disconnected" {
			m[t] = u.Status
		}
	}
	return m
}

// activitySnapshot returns the current activity label per subscribed target
// that is actively working (e.g. "Compacting… 1m 12s", "Stewing… 6m 28s").
// Empty for panes that aren't working or have no activity yet.
func (h *captureHub) activitySnapshot() map[string]string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	m := make(map[string]string, len(h.latest))
	for t, u := range h.latest {
		if u.Status == "working" && u.Activity != "" {
			m[t] = u.Activity
		}
	}
	return m
}

func (h *captureHub) subscribe(target string) chan paneUpdate {
	ch := make(chan paneUpdate, 4)
	h.mu.Lock()
	h.subscribers[target] = append(h.subscribers[target], ch)
	if latest, ok := h.latest[target]; ok && len(latest.Msg) > 0 {
		select {
		case ch <- latest:
		default:
		}
	}
	h.mu.Unlock()
	return ch
}

func (h *captureHub) unsubscribe(target string, ch chan paneUpdate) {
	h.mu.Lock()
	subs := h.subscribers[target]
	for i, s := range subs {
		if s == ch {
			h.subscribers[target] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(h.subscribers[target]) == 0 {
		delete(h.subscribers, target)
		delete(h.termWorkingUntil, target)
	}
	h.mu.Unlock()
	close(ch)
}

func (h *captureHub) run() {
	// Try control mode first (zero subprocess architecture). If it is not
	// available now, or dies later, the loop below runs on batch capture and a
	// supervisor keeps trying to get it back.
	h.connectControl()

	// 25fps. Control mode captures over the persistent connection, batch mode
	// shells out; the batch path throttles itself below.
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	var lastBatch time.Time

	failCounts := map[string]int{}

	for range ticker.C {
		h.mu.RLock()
		targets := make([]string, 0, len(h.subscribers))
		for t := range h.subscribers {
			targets = append(targets, t)
		}
		h.mu.RUnlock()

		if len(targets) == 0 {
			continue
		}

		toCapture := targets

		// Capture pane contents over whichever transport is up right now.
		var captures map[string]string
		if tc := h.control(); tc != nil {
			captures = make(map[string]string, len(toCapture))
			for _, t := range toCapture {
				out, err := tc.capturePaneByTarget(t)
				if err != nil {
					// A dead connection fails every target. Log it once and let
					// the supervisor rebuild rather than repeating it per pane
					// at 25fps.
					if err == errControlDead {
						break
					}
					log.Printf("[hub] capture failed for %s: %v", t, err)
				} else {
					captures[t] = out
				}
			}
		} else {
			// Batch capture forks a shell, so it runs at 10fps, not 25.
			if time.Since(lastBatch) < 100*time.Millisecond {
				continue
			}
			lastBatch = time.Now()
			captures = batchCapture(toCapture)
		}

		for _, target := range toCapture {
			out, ok := captures[target]
			if !ok {
				failCounts[target]++
				if failCounts[target] > 50 {
					h.mu.Lock()
					msg, _ := json.Marshal(map[string]string{"type": "status", "data": "disconnected"})
					update := paneUpdate{Status: "disconnected", Full: false, Msg: msg}
					for _, ch := range h.subscribers[target] {
						select {
						case ch <- update:
						default:
						}
					}
					h.mu.Unlock()
				}
				continue
			}
			delete(failCounts, target)

			// Strip trailing whitespace + truncate decorative lines
			rawLines := strings.Split(out, "\n")
			for i, line := range rawLines {
				line = strings.TrimRight(line, " ")
				plain := ansiRegex.ReplaceAllString(line, "")
				plain = strings.TrimRight(plain, " ")
				stripped := strings.TrimLeft(plain, "─━═╌╍┄┅╶╴ ")
				if len(plain) > 40 && len(stripped) == 0 {
					line = "@@HRULE@@"
				}
				rawLines[i] = line
			}
			content := strings.Join(rawLines, "\n")

			status, activity, permMode := h.resolveStatus(target, content)

			// Get scheduled task info BEFORE locking hub
			var schedInfo interface{}
			if tasks := sched.getTasksForPane(target); len(tasks) > 0 {
				schedInfo = tasks
			}

			h.mu.Lock()
			prev := h.latest[target]
			if content != prev.Msg_content {
				msgData := map[string]interface{}{
					"type":     "content",
					"data":     content,
					"status":   status,
					"activity": activity,
				}
				if permMode != "" {
					msgData["permissionMode"] = permMode
				}
				if schedInfo != nil {
					msgData["scheduled"] = schedInfo
				}
				msg, _ := json.Marshal(msgData)
				update := paneUpdate{Status: status, Activity: activity, Full: true, Msg: msg, Msg_content: content}
				h.latest[target] = update
				for _, ch := range h.subscribers[target] {
					select {
					case ch <- update:
					default:
					}
				}
			} else if status != prev.Status {
				statusMsg := map[string]interface{}{
					"type":     "status-update",
					"status":   status,
					"activity": activity,
				}
				if permMode != "" {
					statusMsg["permissionMode"] = permMode
				}
				msg, _ := json.Marshal(statusMsg)
				update := paneUpdate{Status: status, Activity: activity, Full: false, Msg: msg, Msg_content: prev.Msg_content}
				h.latest[target] = update
				for _, ch := range h.subscribers[target] {
					select {
					case ch <- update:
					default:
					}
				}
			}
			h.mu.Unlock()
		}
	}
}

// batchCapture runs ONE shell command to capture all panes at once.
// Returns map[target] → captured content string.
// Reduces subprocess count from N to 1 per tick.
const captureSep = "@@CWSEP@@"

func batchCapture(targets []string) map[string]string {
	if len(targets) == 0 {
		return nil
	}

	// Build a single shell command: for each target, capture and print separator
	var cmd strings.Builder
	for i, t := range targets {
		if i > 0 {
			cmd.WriteString(" ; ")
		}
		// Use printf for separator (not echo, to avoid newline issues)
		cmd.WriteString("tmux capture-pane -t '")
		cmd.WriteString(t)
		cmd.WriteString("' -e -p 2>/dev/null ; printf '\\n")
		cmd.WriteString(captureSep)
		cmd.WriteString("\\n'")
	}

	out, err := exec.Command("sh", "-c", cmd.String()).Output()
	if err != nil {
		return nil
	}

	// Split output by separator
	parts := strings.Split(string(out), "\n"+captureSep+"\n")
	result := make(map[string]string, len(targets))
	for i, t := range targets {
		if i < len(parts) {
			result[t] = parts[i]
		}
	}
	return result
}

// resolveStatus returns (status, activity, permissionMode) — uses hooks if available, falls back to terminal parsing
func (h *captureHub) resolveStatus(target, content string) (string, string, string) {
	if hooks != nil {
		dir := getPaneDir(target)
		if dir != "" {
			hs := hooks.getStateForPane(target, dir)
			if hs != nil {
				// Stale working/error: if no hook event in 120s, fall through to terminal
				if (hs.Status == "working" || hs.Status == "error") && time.Since(hs.UpdatedAt) > 120*time.Second {
					// fall through to terminal parsing
				} else {
					// Hooks are authoritative for status; terminal is authoritative for permission mode
					mode := parsePermissionMode(content)
					if mode == "" {
						mode = hs.PermissionMode
					}
					activity := hs.Activity
					// While working, prefer the live spinner label ("Compacting… 1m 12s")
					// — it carries the elapsed timer the hook activity lacks.
					if hs.Status == "working" {
						if _, spin := parseTerminalStatus(content); spin != "" {
							activity = spin
						}
					}
					return hs.Status, activity, mode
				}
			}
		}
	}
	// Fallback: detect status from Claude Code terminal output with debounce
	status, activity := parseTerminalStatus(content)
	if status == "working" {
		// Extend debounce: stay "working" for at least 5s after last detection
		h.termWorkingUntil[target] = time.Now().Add(5 * time.Second)
	} else if deadline, ok := h.termWorkingUntil[target]; ok && time.Now().Before(deadline) {
		// Within debounce window: keep showing "working" to prevent flicker
		status = "working"
	}
	// Always try to detect permission mode from terminal (updates faster than hooks)
	mode := parsePermissionMode(content)
	if mode != "" {
		return status, activity, mode
	}
	return status, activity, ""
}

// parseTerminalStatus detects idle vs working from Claude Code terminal content.
// Used when no hook state exists or hook state is stale.
// spinnerRe matches the Claude Code working status line, e.g.
//   "✽ Stewing… (6m 28s · ↓ 12.0k tokens)"
//   "✻ Architecting… (3m 17s · ↓ 10.9k tokens · thinking with xhigh effort)"
//   "✢ Compacting conversation… (17s)"
// Matched by STRUCTURE, not keywords (keyword matching false-fired on any pane
// whose transcript merely contained the word "compacting"):
//   • a leading spinner glyph — Claude cycles ·✢✳✶✻✽… so we allow any single
//     symbol EXCEPT the assistant bullet ⏺, the tool-result ⎿, and box-drawing
//     (those start ordinary output lines that can also end in …);
//   • a Capitalized gerund of up to 3 words (covers "Compacting conversation");
//   • the … char and the REQUIRED "(elapsed…)" parenthetical the status line
//     always carries — its presence is what separates it from prose.
// Group 1 = the verb phrase, group 2 = the parenthetical.
var spinnerRe = regexp.MustCompile(
	`^[^\s\p{L}\p{N}\x{23fa}\x{23bf}\x{2500}-\x{257f}]\s+(\p{Lu}[\p{L}]+(?:\s+[\p{L}]+){0,2})\x{2026}\s*\(([^)]+)\)`)

// progressRe matches the progress-bar line Claude renders under some spinners
// (compaction shows "▰▰▰▱▱▱ 47%"). The bar glyphs (▰ U+25B0 / ▱ U+25B1) anchor
// it so a bare "47%" in prose doesn't match. Group 1 = the percentage.
var progressRe = regexp.MustCompile(`[\x{25b0}\x{25b1}][\x{25b0}\x{25b1}\s]*(\d{1,3})%`)

// spinnerActivity builds a short card label from the spinner verb + the leading
// part of the parenthetical (the elapsed timer, before the first " · ").
// e.g. verb="Compacting", paren="1m 12s · ↓ 3.1k tokens" → "Compacting… 1m 12s".
func spinnerActivity(verb, paren string) string {
	act := verb + "…"
	if paren = strings.TrimSpace(paren); paren != "" {
		if idx := strings.Index(paren, " · "); idx >= 0 {
			paren = paren[:idx]
		}
		if paren = strings.TrimSpace(paren); paren != "" && paren != "esc to interrupt" {
			act += " " + paren
		}
	}
	return act
}

func parseTerminalStatus(content string) (string, string) {
	lines := strings.Split(content, "\n")
	pct := "" // progress-bar % seen below the spinner (compaction), if any
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < 15; i-- {
		plain := ansiRegex.ReplaceAllString(lines[i], "")
		plain = strings.TrimSpace(plain)
		if plain == "" || plain == "@@HRULE@@" {
			continue
		}
		// Skip decorative separator lines
		stripped := strings.TrimLeft(plain, "─━═╌╍┄┅╶╴ ")
		if len(plain) > 40 && len(stripped) == 0 {
			continue
		}
		checked++
		// Progress bar under the spinner (compaction). Scanned first going up,
		// so pct is set by the time we reach the spinner line above it.
		if m := progressRe.FindStringSubmatch(plain); m != nil {
			pct = m[1] + "%"
			continue
		}
		// Working spinner: "<glyph> <Gerund>\u2026 (<elapsed> \u00b7 \u2193 <tokens>[\u00b7 effort])".
		// Claude cycles glyphs (\u00b7\u2722\u2733\u2736\u273b\u273d \u2026), so match the line STRUCTURE.
		if m := spinnerRe.FindStringSubmatch(plain); m != nil {
			// A progress bar (compaction) means the % is the real progress \u2014
			// show it instead of the elapsed timer.
			if pct != "" {
				return "working", m[1] + "\u2026 " + pct
			}
			return "working", spinnerActivity(m[1], m[2])
		}
		// Tool actively running (e.g. "Bash Running\u2026")
		if strings.HasSuffix(plain, "Running\u2026") || strings.HasSuffix(plain, "Running...") {
			return "working", strings.TrimLeft(plain, "\u00b7\u2722\u2733\u2736\u273b\u273d\u2217\u23fa ")
		}
	}
	return "idle", ""
}

// parsePermissionMode detects the Claude Code permission mode from the terminal.
// Scans last ~10 lines for mode keywords. Since we only run on confirmed Claude Code
// panes, absence of a mode keyword means "default" mode.
func parsePermissionMode(content string) string {
	lines := strings.Split(content, "\n")
	start := len(lines) - 10
	if start < 0 {
		start = 0
	}
	for i := len(lines) - 1; i >= start; i-- {
		lower := strings.ToLower(ansiRegex.ReplaceAllString(lines[i], ""))
		if strings.Contains(lower, "bypass permissions") {
			return "bypassPermissions"
		}
		if strings.Contains(lower, "accept edits") {
			return "acceptEdits"
		}
		if strings.Contains(lower, "plan mode") {
			return "plan"
		}
	}
	return "default"
}

// pushHookStatus broadcasts hook-derived status changes to all subscribers
func (h *captureHub) pushHookStatus(hs *hookStore) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for target := range h.subscribers {
		dir := getPaneDir(target)
		if dir == "" {
			continue
		}
		hookState := hs.getStateForPane(target, dir)
		if hookState == nil {
			continue
		}

		prev := h.latest[target]
		if hookState.Status != prev.Status {
			pushMsg := map[string]interface{}{
				"type":     "status-update",
				"status":   hookState.Status,
				"activity": hookState.Activity,
			}
			if hookState.PermissionMode != "" {
				pushMsg["permissionMode"] = hookState.PermissionMode
			}
			msg, _ := json.Marshal(pushMsg)
			update := paneUpdate{
				Status:      hookState.Status,
				Activity:    hookState.Activity,
				Full:        false,
				Msg:         msg,
				Msg_content: prev.Msg_content,
			}
			h.latest[target] = update
			for _, ch := range h.subscribers[target] {
				select {
				case ch <- update:
				default:
				}
			}
		}
	}
}

// ── tmux control-mode supervision ───────────────────────────────────────────
//
// The control connection is the hub's fast path, and it is not durable: a tmux
// server restart, a `kill-server`, or the socket going away all end it. Before
// this was supervised, readLoop simply returned, every capture then blocked for
// its full timeout, and the dashboard froze on its last frame while HTTP kept
// answering 200 — indistinguishable from a crash, but nothing had crashed and
// nothing was logged.

// control returns the live connection, or nil when we are on batch capture.
func (h *captureHub) control() *tmuxControl {
	h.ctlMu.RLock()
	tc := h.ctl
	h.ctlMu.RUnlock()
	if tc != nil && !tc.alive() {
		return nil
	}
	return tc
}

func (h *captureHub) setControl(tc *tmuxControl) {
	h.ctlMu.Lock()
	h.ctl = tc
	h.ctlMu.Unlock()
}

// connectControl attempts one connection. On success it starts the watcher; on
// failure it schedules a retry, so a server started before tmux is ready still
// ends up on the fast path.
func (h *captureHub) connectControl() {
	h.ctlMu.RLock()
	closing := h.closing
	h.ctlMu.RUnlock()
	if closing {
		return
	}

	tc := newTmuxControl()
	if err := tc.start(); err != nil {
		log.Printf("[hub] control mode unavailable, using batch capture: %v", err)
		go h.retryControl()
		return
	}
	h.setControl(tc)
	log.Println("[hub] control mode active (zero-poll)")
	go h.watchControl(tc)
}

// watchControl refreshes the pane map while the connection lives, and reacts
// when it dies.
func (h *captureHub) watchControl(tc *tmuxControl) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-tc.dead:
			h.setControl(nil)
			if tc.stopping.Load() {
				return // we asked for this
			}
			log.Println("[hub] control connection lost — batch capture until it is back")
			go h.retryControl()
			return
		case <-ticker.C:
			tc.refreshPaneMap()
		}
	}
}

// retryControl reconnects with backoff. Capture keeps working on the fallback
// throughout, so this is allowed to be patient.
func (h *captureHub) retryControl() {
	delay := 2 * time.Second
	for {
		time.Sleep(delay)

		h.ctlMu.RLock()
		closing := h.closing
		h.ctlMu.RUnlock()
		if closing {
			return
		}

		tc := newTmuxControl()
		if err := tc.start(); err != nil {
			if delay < 30*time.Second {
				delay *= 2
			}
			log.Printf("[hub] control reconnect failed (%v), retrying in %s", err, delay)
			continue
		}
		h.setControl(tc)
		log.Println("[hub] control mode reconnected")
		go h.watchControl(tc)
		return
	}
}

// shutdown detaches the control-mode client. Every exit path must call this:
// an orphaned `tmux -C attach` outlives the process that spawned it and stays
// on the tmux server as a client, so skipping it leaks one per restart.
func (h *captureHub) shutdown() {
	h.ctlMu.Lock()
	h.closing = true
	tc := h.ctl
	h.ctl = nil
	h.ctlMu.Unlock()
	tc.stop()
}
