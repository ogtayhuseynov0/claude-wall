package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// feedEntry is one item in the activity feed
type feedEntry struct {
	Time      time.Time `json:"time"`
	Session   string    `json:"session"`   // tmux session name
	DirName   string    `json:"dirName"`   // project directory basename
	Branch    string    `json:"branch"`    // git branch
	Target    string    `json:"target"`    // tmux pane target
	Event     string    `json:"event"`     // PreToolUse, PostToolUse, Stop, PermissionRequest, etc.
	Status    string    `json:"status"`    // working, permission, idle
	Activity  string    `json:"activity"`  // human-readable: "$ npm test", "Edit main.go"
	ToolName  string    `json:"toolName"`  // raw tool name
}

// feedStore keeps a ring buffer of recent events
type feedStore struct {
	mu      sync.RWMutex
	entries []feedEntry
	maxSize int
	version int // increments on each add, used for polling
}

func newFeedStore(maxSize int) *feedStore {
	return &feedStore{
		entries: make([]feedEntry, 0, maxSize),
		maxSize: maxSize,
	}
}

func (f *feedStore) add(entry feedEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.entries = append(f.entries, entry)
	if len(f.entries) > f.maxSize {
		f.entries = f.entries[len(f.entries)-f.maxSize:]
	}
	f.version++
}

// getAfter returns entries added after the given version
func (f *feedStore) getAfter(afterVersion int) ([]feedEntry, int) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if afterVersion >= f.version {
		return nil, f.version
	}

	// Calculate how many new entries
	diff := f.version - afterVersion
	if diff > len(f.entries) {
		diff = len(f.entries)
	}

	result := make([]feedEntry, diff)
	copy(result, f.entries[len(f.entries)-diff:])
	return result, f.version
}

// getRecent returns the last N entries
func (f *feedStore) getRecent(n int) []feedEntry {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if n > len(f.entries) {
		n = len(f.entries)
	}
	result := make([]feedEntry, n)
	copy(result, f.entries[len(f.entries)-n:])
	return result
}

// getPending returns entries with status "permission" that aren't stale (>60s).
// Cross-checks hookStore state: if hooks say a pane is "permission", the most
// recent permission entry is returned even if later feed entries overwrote it.
func (f *feedStore) getPending() []feedEntry {
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Track latest status per target AND most recent permission entry
	latest := map[string]*feedEntry{}
	lastPerm := map[string]*feedEntry{}
	for i := range f.entries {
		e := &f.entries[i]
		latest[e.Target] = e
		if e.Status == "permission" {
			lastPerm[e.Target] = e
		}
	}

	var result []feedEntry
	for target, e := range latest {
		if e.Status == "permission" && time.Since(e.Time) < 60*time.Second {
			result = append(result, *e)
			continue
		}
		// Fallback: if hookStore still says "permission", use the last permission entry
		if hooks != nil && lastPerm[target] != nil && time.Since(lastPerm[target].Time) < 60*time.Second {
			dir := getPaneDir(target)
			if dir != "" {
				hs := hooks.getStateForPane(target, dir)
				if hs != nil && hs.Status == "permission" {
					result = append(result, *lastPerm[target])
				}
			}
		}
	}
	return result
}

// clearPending removes the permission status for a target
func (f *feedStore) clearPending(target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Mark the latest entry for this target as idle
	for i := len(f.entries) - 1; i >= 0; i-- {
		if f.entries[i].Target == target && f.entries[i].Status == "permission" {
			f.entries[i].Status = "idle"
			break
		}
	}
	f.version++
}

func (f *feedStore) getVersion() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.version
}

// buildFeedEntry creates a feed entry from a hook event
func buildFeedEntry(event hookEvent) feedEntry {
	cwd := normalizePath(event.CWD)

	session := ""
	dirName := baseName(cwd)
	target := ""

	// Use PaneTarget from TMUX_PANE (most reliable)
	if event.PaneTarget != "" {
		target = event.PaneTarget
		if idx := strings.Index(target, ":"); idx >= 0 {
			session = target[:idx]
		}
		if d := getPaneDir(target); d != "" {
			dirName = baseName(d)
		}
	}

	// Fallback via hookStore session mapping
	if target == "" && hooks != nil {
		hooks.mu.RLock()
		if hs, ok := hooks.sessions[event.SessionID]; ok && hs.PaneTarget != "" {
			target = hs.PaneTarget
			if idx := strings.Index(target, ":"); idx >= 0 {
				session = target[:idx]
			}
			if d := getPaneDir(target); d != "" {
				dirName = baseName(d)
			}
		}
		hooks.mu.RUnlock()
	}

	// Get branch from the pane's directory
	branch := ""
	if d := getPaneDir(target); d != "" {
		branch = gitBranch(d)
	}

	status := ""
	switch event.EventName {
	case "PreToolUse":
		status = "working"
	case "PostToolUse":
		status = "working"
	case "PostToolUseFailure":
		status = "working"
	case "SubagentStart":
		status = "working"
	case "SubagentStop":
		status = "working"
	case "TaskCompleted":
		status = "idle"
	case "StopFailure":
		status = "idle"
	case "Stop":
		status = "idle"
	case "PermissionRequest":
		status = "permission"
	case "Notification":
		// Parse notification type to distinguish permission from idle
		var notif map[string]interface{}
		if json.Unmarshal(event.Notification, &notif) == nil {
			if t, _ := notif["type"].(string); t == "permission_prompt" || t == "elicitation_dialog" {
				status = "permission"
			} else {
				status = "idle"
			}
		} else {
			status = "idle"
		}
	case "SessionStart":
		status = "idle"
	case "SessionEnd":
		status = "idle"
	}

	return feedEntry{
		Time:     time.Now(),
		Session:  session,
		DirName:  dirName,
		Branch:   branch,
		Target:   target,
		Event:    event.EventName,
		Status:   status,
		Activity: formatActivity(event.ToolName, event.ToolInput),
		ToolName: event.ToolName,
	}
}

