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
	mu          sync.RWMutex
	subscribers map[string][]chan paneUpdate // target → channels
	latest      map[string]paneUpdate       // target → last update
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
		subscribers: make(map[string][]chan paneUpdate),
		latest:      make(map[string]paneUpdate),
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

		for _, target := range targets {
			out, err := exec.Command("tmux", "capture-pane", "-t", target, "-e", "-p").Output()
			if err != nil {
				failCounts[target]++
				if failCounts[target] > 50 { // 5 seconds of failures → send disconnect
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
			rawLines := strings.Split(string(out), "\n")
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
			if content != prev.Msg_content {
				msg, _ := json.Marshal(map[string]interface{}{
					"type":     "content",
					"data":     content,
					"status":   status,
					"activity": activity,
				})
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

// resolveStatus returns (status, activity) — uses hooks if available, falls back to terminal parsing
func (h *captureHub) resolveStatus(target, content string) (string, string) {
	if hooks == nil {
		return "idle", ""
	}

	dir := getPaneDir(target)
	if dir == "" {
		return "idle", ""
	}

	hs := hooks.getStateForPane(target, dir)
	if hs == nil {
		// No hooks ever received for this pane — show idle (don't guess from terminal)
		return "idle", ""
	}

	// Hooks are authoritative. Trust the last hook state.
	// Only exception: if permission hook is old (>30s) and a Stop/PostToolUse came after,
	// the hook store already updated the state. So just return what hooks say.
	return hs.Status, hs.Activity
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
