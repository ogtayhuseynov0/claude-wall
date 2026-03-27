package main

import (
	"encoding/json"
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
	latest           map[string]paneUpdate       // target → last update
	termWorkingUntil map[string]time.Time        // target → debounce: stay "working" until this time
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
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	failCounts := map[string]int{} // track consecutive capture failures

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

		// Batch capture: ONE subprocess for all panes
		captures := batchCapture(targets)

		for _, target := range targets {
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
				// Strip ANSI codes to check if line is purely decorative
				plain := ansiRegex.ReplaceAllString(line, "")
				plain = strings.TrimRight(plain, " ")
				stripped := strings.TrimLeft(plain, "─━═╌╍┄┅╶╴ ")
				if len(plain) > 40 && len(stripped) == 0 {
					// Replace with a marker that the client renders as full-width
					line = "@@HRULE@@"
				}
				rawLines[i] = line
			}
			content := strings.Join(rawLines, "\n")

			// Determine status: prefer hooks, fall back to terminal parsing
			status, activity := h.resolveStatus(target, content)

			h.mu.Lock()
			prev := h.latest[target]
			// Get scheduled task info for this pane
			var schedInfo interface{}
			if tasks := sched.getTasksForPane(target); len(tasks) > 0 {
				schedInfo = tasks
			}

			if content != prev.Msg_content {
				msgData := map[string]interface{}{
					"type":     "content",
					"data":     content,
					"status":   status,
					"activity": activity,
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
				// Only broadcast when STATUS changes (not activity-only changes)
				// Activity updates piggyback on content updates above
				msg, _ := json.Marshal(map[string]interface{}{
					"type":     "status-update",
					"status":   status,
					"activity": activity,
				})
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

// resolveStatus returns (status, activity) — uses hooks if available, falls back to terminal parsing
func (h *captureHub) resolveStatus(target, content string) (string, string) {
	if hooks != nil {
		dir := getPaneDir(target)
		if dir != "" {
			hs := hooks.getStateForPane(target, dir)
			if hs != nil {
				// Stale working/error: if no hook event in 120s, fall through to terminal
				if (hs.Status == "working" || hs.Status == "error") && time.Since(hs.UpdatedAt) > 120*time.Second {
					// fall through to terminal parsing
				} else {
					// Hooks are authoritative
					return hs.Status, hs.Activity
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
	return status, activity
}

// parseTerminalStatus detects idle vs working from Claude Code terminal content.
// Used when no hook state exists or hook state is stale.
func parseTerminalStatus(content string) (string, string) {
	lines := strings.Split(content, "\n")
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
		// Thinking spinner
		if strings.HasPrefix(plain, "\u2733") {
			return "working", ""
		}
		// Tool actively running
		if strings.HasSuffix(plain, "Running\u2026") || strings.HasSuffix(plain, "Running...") {
			return "working", ""
		}
	}
	return "idle", ""
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
			msg, _ := json.Marshal(map[string]interface{}{
				"type":     "status-update",
				"status":   hookState.Status,
				"activity": hookState.Activity,
			})
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
